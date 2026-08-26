/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
use async_trait::async_trait;
use carbide_instrument::{Event, emit};

use crate::SecretsError;
use crate::credentials::{CredentialKey, CredentialReader, Credentials};

pub struct ChainedCredentialReader(Vec<Box<dyn CredentialReader>>);

/// Operator action shared by authoritative-file read and mutation errors.
pub const UFM_LOCAL_CREDENTIAL_REMEDIATION: &str =
    // xtask:allow-error-case: UFM and NICo are product names
    "when environment credentials supply the UFM fabric, update them and restart NICo; otherwise \
     update the watched credential file (environment credentials take precedence over the file); \
     for a staged migration, set credentials.file.allow_legacy_ufm_fallback = true to enable \
     legacy backend access";

/// Stops UFM credential lookup before the persistent backend portion of a
/// reader chain. Place this after local environment and file readers.
#[derive(Debug)]
pub struct UfmCredentialFallbackBlocker;

#[derive(Event)]
#[event(
    event_name = "ufm_local_credential_missing",
    metric_name = "carbide_ufm_local_credential_missing_total",
    component = "nico-api",
    log = error,
    metric = counter,
    message = "authoritative local UFM credential is missing",
    describe = "Number of UFM credential lookups rejected because authoritative local sources lack the configured fabric."
)]
struct UfmLocalCredentialMissing {
    #[context]
    fabric: String,
}

#[async_trait]
impl CredentialReader for UfmCredentialFallbackBlocker {
    async fn get_credentials(
        &self,
        key: &CredentialKey,
    ) -> Result<Option<Credentials>, SecretsError> {
        let CredentialKey::UfmAuth { fabric } = key else {
            return Ok(None);
        };

        emit(UfmLocalCredentialMissing {
            fabric: fabric.clone(),
        });
        Err(SecretsError::UfmCredentialReadBlocked {
            fabric: fabric.clone(),
        })
    }
}

impl From<Vec<Box<dyn CredentialReader>>> for ChainedCredentialReader {
    fn from(providers: Vec<Box<dyn CredentialReader>>) -> Self {
        Self(providers)
    }
}

#[async_trait]
impl CredentialReader for ChainedCredentialReader {
    async fn get_credentials(
        &self,
        key: &CredentialKey,
    ) -> Result<Option<Credentials>, SecretsError> {
        for provider in &self.0 {
            if let Some(credentials) = provider.get_credentials(key).await? {
                return Ok(Some(credentials));
            }
        }
        Ok(None)
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::time::Duration;

    use carbide_instrument::testing::MetricsCapture;
    use serial_test::serial;

    use super::*;
    use crate::credentials::CredentialType;
    use crate::local_credentials::{
        EnvCredentials, EnvCredentialsConfig, FileCredentialsConfig, FileCredentialsWatcher,
    };
    use crate::test_support::credentials::TestCredentialManager;

    #[tokio::test]
    async fn empty_chain_returns_none() {
        let chain: ChainedCredentialReader = vec![].into();
        let key = CredentialKey::DpuUefi {
            credential_type: CredentialType::SiteDefault,
        };
        let value = chain.get_credentials(&key).await.expect("empty chain");
        assert_eq!(value, None);
    }

    #[tokio::test]
    async fn chained_reader_falls_through_to_last_provider() {
        let vault = Arc::new(TestCredentialManager::new(Credentials::UsernamePassword {
            username: "vault-user".to_string(),
            password: "vault-pass".to_string(),
        }));

        let chain: ChainedCredentialReader = vec![
            Box::new(
                EnvCredentials::new(EnvCredentialsConfig {
                    prefix: Some("CARBIDE_TEST_FALLTHRU_".to_string()),
                    ..Default::default()
                })
                .expect("create env provider"),
            ) as Box<dyn CredentialReader>,
            Box::new(vault),
        ]
        .into();

        let key = CredentialKey::DpuUefi {
            credential_type: CredentialType::SiteDefault,
        };
        let value = chain.get_credentials(&key).await.expect("get credentials");

        assert_eq!(
            value,
            Some(Credentials::UsernamePassword {
                username: "vault-user".to_string(),
                password: "vault-pass".to_string(),
            })
        );
    }

    #[tokio::test]
    async fn ufm_fallback_blocker_stops_lookup_before_backend() {
        let metrics = MetricsCapture::start();
        let backend = Arc::new(TestCredentialManager::new(Credentials::UsernamePassword {
            username: "vault-user".to_string(),
            password: "vault-pass".to_string(),
        }));
        let chain: ChainedCredentialReader = vec![
            Box::new(UfmCredentialFallbackBlocker) as Box<dyn CredentialReader>,
            Box::new(backend),
        ]
        .into();
        let key = CredentialKey::UfmAuth {
            fabric: "fabric-a".to_string(),
        };

        let error = chain
            .get_credentials(&key)
            .await
            .expect_err("UFM lookup must stop before the legacy backend");

        let message = error.to_string();
        assert!(matches!(
            error,
            SecretsError::UfmCredentialReadBlocked { ref fabric } if fabric == "fabric-a"
        ));
        assert!(message.contains("fabric-a"));
        assert!(message.contains(UFM_LOCAL_CREDENTIAL_REMEDIATION));
        assert_eq!(
            metrics.counter_delta("carbide_ufm_local_credential_missing_total", &[]),
            1.0,
            "the fabric identifier must remain log context rather than a metric label:\n{}",
            metrics.render()
        );
    }

    #[tokio::test]
    async fn ufm_fallback_blocker_allows_other_credentials_to_continue() {
        let expected = Credentials::UsernamePassword {
            username: "vault-user".to_string(),
            password: "vault-pass".to_string(),
        };
        let backend = Arc::new(TestCredentialManager::new(expected.clone()));
        let chain: ChainedCredentialReader = vec![
            Box::new(UfmCredentialFallbackBlocker) as Box<dyn CredentialReader>,
            Box::new(backend),
        ]
        .into();
        let key = CredentialKey::DpuUefi {
            credential_type: CredentialType::SiteDefault,
        };

        let value = chain
            .get_credentials(&key)
            .await
            .expect("non-UFM lookup must continue to the backend");

        assert_eq!(value, Some(expected));
    }

    #[tokio::test]
    #[serial]
    async fn env_takes_precedence_over_file_and_vault() {
        let dir = tempfile::tempdir().expect("create temp dir");
        let file_path = dir.path().join("static-creds.yaml");
        tokio::fs::write(
            &file_path,
            r#"dpu_uefi_site_default:
  username: file-user
  password: file-password
"#,
        )
        .await
        .expect("write static credential file");

        let vault = Arc::new(TestCredentialManager::new(Credentials::UsernamePassword {
            username: "vault-user".to_string(),
            password: "vault-password".to_string(),
        }));
        let key = CredentialKey::DpuUefi {
            credential_type: CredentialType::SiteDefault,
        };

        let env_user = "CARBIDE_TEST_PREC__DPU_UEFI_SITE_DEFAULT__USERNAME";
        let env_password = "CARBIDE_TEST_PREC__DPU_UEFI_SITE_DEFAULT__PASSWORD";
        // SAFETY: Initial lint enablement: `#[serial]` serializes participating tests,
        // but it cannot prove Unix process-wide exclusion from unmarked environment
        // readers. This needs owner review.
        unsafe {
            std::env::set_var(env_user, "env-user");
            std::env::set_var(env_password, "env-password");
        }

        let chain: ChainedCredentialReader = vec![
            Box::new(
                EnvCredentials::new(EnvCredentialsConfig {
                    prefix: Some("CARBIDE_TEST_PREC_".to_string()),
                    ..Default::default()
                })
                .expect("create env provider"),
            ) as Box<dyn CredentialReader>,
            Box::new(
                FileCredentialsWatcher::new(FileCredentialsConfig {
                    path: Some(file_path.clone()),
                    poll_interval: Some(Duration::from_millis(250)),
                    ..Default::default()
                })
                .await
                .expect("create file provider"),
            ),
            Box::new(vault.clone()),
        ]
        .into();

        let env_value = chain.get_credentials(&key).await.expect("get env value");
        assert_eq!(
            env_value,
            Some(Credentials::UsernamePassword {
                username: "env-user".to_string(),
                password: "env-password".to_string(),
            })
        );

        // SAFETY: Initial lint enablement: `#[serial]` serializes participating tests,
        // but it cannot prove Unix process-wide exclusion from unmarked environment
        // readers. This needs owner review.
        unsafe {
            std::env::remove_var(env_user);
            std::env::remove_var(env_password);
        }
    }

    #[tokio::test]
    async fn file_takes_precedence_over_vault() {
        let dir = tempfile::tempdir().expect("create temp dir");
        let file_path = dir.path().join("static-creds.yaml");
        tokio::fs::write(
            &file_path,
            r#"dpu_uefi_site_default:
  username: file-user
  password: file-password
"#,
        )
        .await
        .expect("write static credential file");

        let vault = Arc::new(TestCredentialManager::new(Credentials::UsernamePassword {
            username: "vault-user".to_string(),
            password: "vault-password".to_string(),
        }));
        let key = CredentialKey::DpuUefi {
            credential_type: CredentialType::SiteDefault,
        };

        let chain: ChainedCredentialReader = vec![
            Box::new(
                FileCredentialsWatcher::new(FileCredentialsConfig {
                    path: Some(file_path),
                    poll_interval: Some(Duration::from_millis(250)),
                    ..Default::default()
                })
                .await
                .expect("create file provider"),
            ) as Box<dyn CredentialReader>,
            Box::new(vault),
        ]
        .into();

        let file_value = chain.get_credentials(&key).await.expect("get file value");
        assert_eq!(
            file_value,
            Some(Credentials::UsernamePassword {
                username: "file-user".to_string(),
                password: "file-password".to_string(),
            })
        );
    }
}
