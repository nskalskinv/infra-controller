// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package processors

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/NVIDIA/infra-controller/rest-api/auth/pkg/config"
	"github.com/NVIDIA/infra-controller/rest-api/common/pkg/roles"
	cutil "github.com/NVIDIA/infra-controller/rest-api/common/pkg/util"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	cdbm "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db/model"
	userActivity "github.com/NVIDIA/infra-controller/rest-api/workflow/pkg/activity/user"
	freelru "github.com/elastic/go-freelru"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
)

// Key classification

type keyFormat string

const (
	formatNvapi     keyFormat = "nvapi"
	formatLegacy    keyFormat = "legacy"
	nvapiPrefix               = "nvapi-"
	nvapiPayloadLen           = 64
)

var errInvalidKeyFormat = errors.New("credential does not match any accepted NGC API key format")

func detectAPIKeyType(raw string) (keyFormat, error) {
	if payload, ok := strings.CutPrefix(raw, nvapiPrefix); ok {
		if len(payload) != nvapiPayloadLen {
			return "", errInvalidKeyFormat
		}
		return formatNvapi, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", errInvalidKeyFormat
	}

	clientID, tail, found := strings.Cut(string(decoded), ":")
	if !found || clientID == "" || uuid.Validate(tail) != nil {
		return "", errInvalidKeyFormat
	}

	return formatLegacy, nil
}

// NGC lookups

const (
	ngcBaseURL   = "https://api.ngc.nvidia.com"
	fetchTimeout = 10 * time.Second

	keyTypeService  = "SERVICE_KEY"
	keyTypePersonal = "PERSONAL_KEY"
)

var (
	errNgcUnauthorized = errors.New("NGC rejected the API key")
	errNgcUpstream     = errors.New("NGC could not be reached or returned an unusable response")
)

// productRoles maps an NGC service-key product to the NICo role it grants
var productRoles = map[string]string{
	"forge-provider": roles.ProviderAdminRole,
	"forge-tenant":   roles.TenantAdminRole,
}

type ngcClient struct {
	http    *http.Client
	baseURL string
}

type sakResource struct {
	ID string `json:"id"`
}

type sakPolicy struct {
	Product   string        `json:"product"`
	Resources []sakResource `json:"resources"`
}

type sakInfo struct {
	APIKey struct {
		KeyID    string      `json:"keyId"`
		Policies []sakPolicy `json:"policies"`
	} `json:"apiKey"`
}

func (si *sakInfo) toOrgData() cdbm.OrgData {
	orgData := cdbm.OrgData{}

	for _, policy := range si.APIKey.Policies {
		role, mapped := productRoles[policy.Product]
		if !mapped {
			continue
		}

		for _, resource := range policy.Resources {
			orgName, _, _ := strings.Cut(resource.ID, "/")
			if orgName == "" || orgName == "*" {
				continue
			}

			org, found := orgData[orgName]
			if !found {
				org = cdbm.Org{Name: orgName, Roles: []string{}, Teams: []cdbm.Team{}}
			}
			if !slices.Contains(org.Roles, role) {
				org.Roles = append(org.Roles, role)
			}
			orgData[orgName] = org
		}
	}

	return orgData
}

// callerInfo is the subset of get-caller-info this path needs. For a personal key the
// embedded user record is complete, so no further NGC call is required.
type callerInfo struct {
	KeyType string                `json:"type"`
	User    *userActivity.NgcUser `json:"user"`
}

// postCredential sends the credential in a form body, which is how both /v3/keys
// endpoints accept it
func (cl *ngcClient) postCredential(ctx context.Context, path, key string) ([]byte, error) {
	form := url.Values{"credentials": {key}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cl.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNgcUpstream, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return cl.do(req)
}

func (cl *ngcClient) getCallerInfo(ctx context.Context, key string) (*callerInfo, error) {
	body, err := cl.postCredential(ctx, "/v3/keys/get-caller-info", key)
	if err != nil {
		return nil, err
	}

	info := &callerInfo{}
	if err := json.Unmarshal(body, info); err != nil {
		return nil, fmt.Errorf("%w: could not decode get-caller-info response", errNgcUpstream)
	}

	return info, nil
}

func (cl *ngcClient) getSAKInfo(ctx context.Context, key string) (*sakInfo, error) {
	body, err := cl.postCredential(ctx, "/v3/keys/get-sak-info", key)
	if err != nil {
		return nil, err
	}

	info := &sakInfo{}
	if err := json.Unmarshal(body, info); err != nil {
		return nil, fmt.Errorf("%w: could not decode get-sak-info response", errNgcUpstream)
	}

	return info, nil
}

func (cl *ngcClient) getNgcUser(ctx context.Context, key string) (*userActivity.NgcUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cl.baseURL+"/v2/users/me", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNgcUpstream, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept-Encoding", "identity")

	body, err := cl.do(req)
	if err != nil {
		return nil, err
	}

	resp := &userActivity.NgcUserResponse{}
	if err := json.Unmarshal(body, resp); err != nil {
		return nil, fmt.Errorf("%w: could not decode users/me response", errNgcUpstream)
	}

	if resp.RequestStatus.StatusCode != userActivity.NgcRequestStatusSuccess {
		return nil, fmt.Errorf("%w: users/me reported status %s", errNgcUpstream, resp.RequestStatus.StatusCode)
	}

	return &resp.User, nil
}

// do never includes the credential in the returned error, since these errors are logged
func (cl *ngcClient) do(req *http.Request) ([]byte, error) {
	resp, err := cl.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNgcUpstream, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: status %d", errNgcUnauthorized, resp.StatusCode)
	default:
		return nil, fmt.Errorf("%w: status %d", errNgcUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNgcUpstream, err)
	}

	return body, nil
}

// Credential cache

const (
	apiKeyStalePeriod = 5 * time.Minute
	blockLifetime     = 5 * time.Minute

	allowCapacity uint32 = 8192
	blockCapacity uint32 = 4096
)

type apiKeyCache struct {
	allowLRU *freelru.SyncedLRU[[32]byte, uuid.UUID]
	blockLRU *freelru.SyncedLRU[[32]byte, struct{}]
}

func newAPIKeyCache() (*apiKeyCache, error) {
	allowLRU, err := freelru.NewSynced[[32]byte, uuid.UUID](allowCapacity, hashDigest)
	if err != nil {
		return nil, err
	}

	blockLRU, err := freelru.NewSynced[[32]byte, struct{}](blockCapacity, hashDigest)
	if err != nil {
		return nil, err
	}

	// Only the block cache expires. digest -> userID is immutable, so the allow
	// cache is bounded by LRU eviction alone and user freshness comes from the DB row.
	blockLRU.SetLifetime(blockLifetime)

	return &apiKeyCache{allowLRU: allowLRU, blockLRU: blockLRU}, nil
}

func digest(raw string) [32]byte {
	return sha256.Sum256([]byte(raw))
}

func hashDigest(dg [32]byte) uint32 {
	return uint32(dg[0]) | uint32(dg[1])<<8 | uint32(dg[2])<<16 | uint32(dg[3])<<24
}

func (ca *apiKeyCache) allowed(dg [32]byte) (uuid.UUID, bool) {
	return ca.allowLRU.Get(dg)
}

func (ca *apiKeyCache) allow(dg [32]byte, userID uuid.UUID) {
	ca.allowLRU.Add(dg, userID)
}

func (ca *apiKeyCache) blocked(dg [32]byte) bool {
	_, found := ca.blockLRU.Get(dg)
	return found
}

func (ca *apiKeyCache) block(dg [32]byte) {
	ca.blockLRU.Add(dg, struct{}{})
	// Drop any allow mapping so a revoked key stops costing an NGC call per block lifetime
	ca.allowLRU.Remove(dg)
}

// Resolution

var (
	errKeyRejected  = errors.New("API key is not valid")
	errUnresolvable = errors.New("API key could not be resolved")
)

type identity struct {
	starfleetID *string
	auxiliaryID *string
	email       string
	firstName   string
	lastName    string
	orgData     cdbm.OrgData
}

type resolver struct {
	dbSession *cdb.Session
	ngc       *ngcClient
	cache     *apiKeyCache
	flight    singleflight.Group
}

func newResolver(dbSession *cdb.Session) (*resolver, error) {
	cache, err := newAPIKeyCache()
	if err != nil {
		return nil, err
	}

	return &resolver{
		dbSession: dbSession,
		ngc: &ngcClient{
			http:    &http.Client{Timeout: fetchTimeout},
			baseURL: ngcBaseURL,
		},
		cache: cache,
	}, nil
}

func (r *resolver) resolve(ctx context.Context, raw string) (*cdbm.User, error) {
	dg := digest(raw)

	format, err := detectAPIKeyType(raw)
	if err != nil {
		r.cache.block(dg)
		return nil, errKeyRejected
	}

	if r.cache.blocked(dg) {
		return nil, errKeyRejected
	}

	if userID, found := r.cache.allowed(dg); found {
		userDAO := cdbm.NewUserDAO(r.dbSession)
		dbUser, err := userDAO.Get(ctx, nil, userID, nil)
		if err == nil && dbUser.OrgData != nil && time.Since(dbUser.Updated) <= apiKeyStalePeriod {
			return dbUser, nil
		}
	}

	return r.refresh(ctx, dg, raw, format)
}

func (r *resolver) refresh(ctx context.Context, dg [32]byte, raw string, format keyFormat) (*cdbm.User, error) {
	// The leader runs the fetch on its own request goroutine, so it must not be
	// cancelled by that client disconnecting while other callers wait on the result
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()

	dbUser, err, _ := r.flight.Do(string(dg[:]), func() (interface{}, error) {
		id, err := r.fetchIdentity(fetchCtx, format, raw)
		if err != nil {
			if errors.Is(err, errNgcUnauthorized) {
				r.cache.block(dg)
				return nil, errKeyRejected
			}
			return nil, errUnresolvable
		}

		user, err := r.createOrUpdateUser(fetchCtx, id)
		if err != nil {
			return nil, errUnresolvable
		}

		// Must stay inside the flight so waiters observe the mapping
		r.cache.allow(dg, user.ID)
		return user, nil
	})
	if err != nil {
		return nil, err
	}

	user, ok := dbUser.(*cdbm.User)
	if !ok {
		return nil, errUnresolvable
	}

	return user, nil
}

func (r *resolver) fetchIdentity(ctx context.Context, format keyFormat, raw string) (*identity, error) {
	// A legacy key is not an NGC API key record, so it is only resolvable as its owner
	if format == formatLegacy {
		ngcUser, err := r.ngc.getNgcUser(ctx, raw)
		if err != nil {
			return nil, err
		}
		return identityFromNgcUser(ngcUser)
	}

	caller, err := r.ngc.getCallerInfo(ctx, raw)
	if err != nil {
		return nil, err
	}

	switch caller.KeyType {
	case keyTypePersonal:
		if caller.User == nil {
			return nil, fmt.Errorf("%w: get-caller-info returned no user for a personal key", errNgcUpstream)
		}
		return identityFromNgcUser(caller.User)
	case keyTypeService:
		return r.serviceIdentity(ctx, raw)
	default:
		return nil, fmt.Errorf("%w: unrecognized API key type %q", errNgcUpstream, caller.KeyType)
	}
}

// serviceIdentity fetches the per-key SAK record. get-caller-info cannot stand in for it:
// the userId it reports for a service key is the org's cloud account, shared by every
// service key in that org, so it cannot separate two keys holding different roles.
func (r *resolver) serviceIdentity(ctx context.Context, raw string) (*identity, error) {
	info, err := r.ngc.getSAKInfo(ctx, raw)
	if err != nil {
		return nil, err
	}

	if info.APIKey.KeyID == "" {
		return nil, fmt.Errorf("%w: get-sak-info returned no keyId", errNgcUpstream)
	}

	return &identity{
		auxiliaryID: cutil.GetPtr(info.APIKey.KeyID),
		orgData:     info.toOrgData(),
	}, nil
}

func identityFromNgcUser(ngcUser *userActivity.NgcUser) (*identity, error) {
	if ngcUser.StarfleetID == "" {
		return nil, fmt.Errorf("%w: NGC returned a user with no starfleetId", errNgcUpstream)
	}

	firstName, lastName, _ := strings.Cut(ngcUser.Name, " ")

	return &identity{
		starfleetID: cutil.GetPtr(ngcUser.StarfleetID),
		email:       ngcUser.Email,
		firstName:   firstName,
		lastName:    lastName,
		orgData:     userActivity.GetOrgData(ngcUser),
	}, nil
}

func (r *resolver) createOrUpdateUser(ctx context.Context, id *identity) (*cdbm.User, error) {
	userDAO := cdbm.NewUserDAO(r.dbSession)

	dbUser, _, err := userDAO.GetOrCreate(ctx, nil, cdbm.UserGetOrCreateInput{
		StarfleetID: id.starfleetID,
		AuxiliaryID: id.auxiliaryID,
	})
	if err != nil {
		return nil, err
	}

	orgData := id.orgData
	if orgData == nil {
		orgData = cdbm.OrgData{}
	}

	// Always update, even when nothing changed, so "updated" advances and other
	// replicas can see that this credential was just verified against NGC
	input := cdbm.UserUpdateInput{
		UserID:  dbUser.ID,
		OrgData: orgData,
	}
	if id.email != "" {
		input.Email = cutil.GetPtr(id.email)
	}
	if id.firstName != "" {
		input.FirstName = cutil.GetPtr(id.firstName)
	}
	if id.lastName != "" {
		input.LastName = cutil.GetPtr(id.lastName)
	}

	return userDAO.Update(ctx, nil, input)
}

// Ensure KasOriginProcessor implements config.TokenProcessor interface
var _ config.TokenProcessor = (*KasOriginProcessor)(nil)

// KasOriginProcessor processes NGC API keys presented as bearer credentials
type KasOriginProcessor struct {
	resolver *resolver
}

// NewKasOriginProcessor creates a new NGC API key processor
func NewKasOriginProcessor(dbSession *cdb.Session) (config.TokenProcessor, error) {
	res, err := newResolver(dbSession)
	if err != nil {
		return nil, err
	}

	return &KasOriginProcessor{resolver: res}, nil
}

// ProcessToken resolves an NGC API key to a user record, refreshing it from NGC when stale
func (h *KasOriginProcessor) ProcessToken(c echo.Context, tokenStr string, _ *config.JwksConfig, logger zerolog.Logger) (*cdbm.User, *cutil.APIError) {
	dbUser, err := h.resolver.resolve(c.Request().Context(), tokenStr)
	if err != nil {
		if errors.Is(err, errKeyRejected) {
			logger.Warn().Msg("rejected API key in authorization header")
			return nil, cutil.NewAPIError(http.StatusUnauthorized, "Invalid authorization token in request", nil)
		}
		logger.Error().Err(err).Msg("failed to resolve API key against NGC")
		return nil, cutil.NewAPIError(http.StatusServiceUnavailable, "Failed to verify authorization token, try again later", nil)
	}

	// NGC API keys are never NICo service accounts, whatever the NGC key type says
	config.SetIsServiceAccountInContext(c, false)

	c.Set("user", dbUser)
	return dbUser, nil
}
