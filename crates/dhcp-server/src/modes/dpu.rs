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
use carbide_rpc_utils::dhcp::{HostConfig, InterfaceInfo, InterfaceInfoV6};
use carbide_uuid::machine::MachineInterfaceId;
use lru::LruCache;
use rpc::forge::{DhcpDiscovery, DhcpRecord, MessageKind};
use tonic::async_trait;

use super::{DhcpMode, V6Outcome, v6_message_kind};
use crate::Config;
use crate::cache::CacheEntry;
use crate::errors::DhcpError;
use crate::packet_handler::DecodedPacket;

#[derive(Debug)]
pub struct Dpu {}

fn from_host_conf(value: &InterfaceInfo, interface_id: MachineInterfaceId) -> DhcpRecord {
    // Fill only needed fields. Rest are left empty or none.
    DhcpRecord {
        machine_id: None,
        machine_interface_id: Some(interface_id),
        segment_id: None,
        subdomain_id: None,
        fqdn: value.fqdn.clone(),
        mac_address: "dummy".to_string(),
        address: value.address.to_string(),
        mtu: 0,
        prefix: value.prefix.clone(),
        gateway: Some(value.gateway.to_string()),
        booturl: value.booturl.clone(),
        last_invalidation_time: None,
        ntp_servers: vec![],
    }
}

/// Build the family-neutral API record consumed by the DHCPv6 encoder.
fn from_host_conf_v6(
    value: &InterfaceInfo,
    ipv6: &InterfaceInfoV6,
    interface_id: MachineInterfaceId,
) -> Result<DhcpRecord, DhcpError> {
    let mtu = value
        .mtu
        .map(i32::try_from)
        .transpose()
        .map_err(|_| DhcpError::InvalidInput("DHCPv6 MTU exceeds the API range".to_string()))?
        .unwrap_or_default();

    Ok(DhcpRecord {
        machine_id: None,
        machine_interface_id: Some(interface_id),
        segment_id: None,
        subdomain_id: None,
        fqdn: value.fqdn.clone(),
        mac_address: String::new(),
        address: ipv6
            .address
            .map_or_else(String::new, |address| address.to_string()),
        mtu,
        prefix: ipv6.prefix.clone(),
        gateway: None,
        booturl: None,
        last_invalidation_time: None,
        ntp_servers: vec![],
    })
}

#[async_trait]
impl DhcpMode for Dpu {
    async fn discover_dhcp(
        &self,
        discovery_request: DhcpDiscovery,
        config: &Config,
        _machine_cache: &mut std::sync::Arc<tokio::sync::Mutex<LruCache<String, CacheEntry>>>,
    ) -> Result<DhcpRecord, DhcpError> {
        let Some(circuit_id) = discovery_request.circuit_id else {
            return Err(DhcpError::MissingArgument(
                "Missing circuit id.".to_string(),
            ));
        };

        let ip_details = config
            .host_config
            .as_ref()
            .ok_or_else(|| DhcpError::InvalidInput("host input is invalid.".to_string()))?
            .host_ip_addresses
            .get(&circuit_id)
            .ok_or_else(|| {
                DhcpError::MissingArgument(format!("Could not find IP details for {circuit_id}"))
            })?;

        let Some(host_config) = &config.host_config else {
            return Err(DhcpError::MissingArgument(
                "host_config is missing.".to_string(),
            ));
        };

        Ok(from_host_conf(ip_details, host_config.host_interface_id))
    }

    /// Resolve DHCPv6 directly from the interface block delivered in host.yaml.
    async fn discover_dhcp_v6(
        &self,
        discovery_request: DhcpDiscovery,
        config: &Config,
        _machine_cache: &mut std::sync::Arc<tokio::sync::Mutex<LruCache<String, CacheEntry>>>,
    ) -> Result<V6Outcome, DhcpError> {
        let message_kind = v6_message_kind(&discovery_request)?;
        let Some(circuit_id) = discovery_request.circuit_id else {
            return Err(DhcpError::MissingArgument("DHCPv6 circuit id".to_string()));
        };

        // DPU mode selects the precomputed host entry by the receiving interface,
        // matching its DHCPv4 path.
        let host_config = config
            .host_config
            .as_ref()
            .ok_or_else(|| DhcpError::InvalidInput("host input is invalid".to_string()))?;
        let interface = host_config
            .host_ip_addresses
            .get(&circuit_id)
            .ok_or_else(|| {
                DhcpError::MissingArgument(format!("could not find IP details for {circuit_id}"))
            })?;
        let Some(ipv6) = interface.ipv6.as_ref() else {
            // No IPv6 block means this interface is v6-disabled. This is
            // distinct from a SLAAC-only block whose address is absent.
            // TODO(ipv6-only): InterfaceInfo still requires IPv4 fields, so an
            // IPv6-only host needs a synthetic v4 entry until rpc-utils relaxes.
            return Err(DhcpError::MissingArgument(
                "IPv6 interface config".to_string(),
            ));
        };

        let record = || from_host_conf_v6(interface, ipv6, host_config.host_interface_id);

        match message_kind {
            // Stateless requests receive configuration regardless of whether
            // the interface also owns a stateful address.
            MessageKind::V6InfoRequest => Ok(V6Outcome::OptionsOnly(record()?)),
            MessageKind::V6Solicit | MessageKind::V6Request if ipv6.address.is_some() => {
                Ok(V6Outcome::Stateful(record()?))
            }
            // A present prefix with no address is the explicit SLAAC-only
            // contract. The encoder retains the wire type to choose the RFC status.
            MessageKind::V6Solicit | MessageKind::V6Request => Ok(V6Outcome::NoAddress),
            _ => Err(DhcpError::InvalidInput(
                "non-DHCPv6 message kind passed to DHCPv6 mode".to_string(),
            )),
        }
    }

    /// Here circuit is interface name. This is what dhcp-relay used to fill.
    fn get_circuit_id(&self, _packet: &DecodedPacket, circuit_id: &str) -> Option<String> {
        Some(circuit_id.to_string())
    }

    fn should_be_relayed(&self) -> bool {
        false
    }
}

/// This config is fetched by dpu-agent from controller periodically. In case of any change in
/// this configuration, dpu-agent MUST restart dhcp-server.
pub async fn get_host_config(
    host_config_path: Option<String>,
) -> Result<Option<HostConfig>, DhcpError> {
    let Some(host_config) = host_config_path else {
        return Err(DhcpError::MissingArgument(
            "--host_config is missing.".to_string(),
        ));
    };

    let f = tokio::fs::read_to_string(host_config).await?;
    let host_config: HostConfig = serde_yaml::from_str(&f)?;

    Ok(Some(host_config))
}
