use std::fs;
use std::io;
use std::path::{Path, PathBuf};

use anyhow::{Context, anyhow, bail};
use serde::{Deserialize, Serialize};
use toml_edit::{DocumentMut, value};
use url::Url;

use crate::accounts::CodexStore;
use crate::agents::opencode::write_atomic_private;
use crate::tenant;

pub const LOCAL_BASE_URL: &str = "http://127.0.0.1:31415";

#[derive(Clone, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ServerConfig {
    pub name: String,
    pub url: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub gcp_project: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub gcp_zone: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub gcp_instance: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub admin_token: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub tenant_key: String,
}

impl std::fmt::Debug for ServerConfig {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("ServerConfig")
            .field("name", &self.name)
            .field("url", &self.url)
            .field("gcp_project", &self.gcp_project)
            .field("gcp_zone", &self.gcp_zone)
            .field("gcp_instance", &self.gcp_instance)
            .field(
                "admin_token",
                &if self.admin_token.is_empty() {
                    "<empty>"
                } else {
                    "<redacted>"
                },
            )
            .field(
                "tenant_key",
                &if self.tenant_key.is_empty() {
                    "<empty>"
                } else {
                    "<redacted>"
                },
            )
            .finish()
    }
}

impl ServerConfig {
    pub fn validate(&self) -> anyhow::Result<()> {
        if self.name.trim().is_empty() {
            bail!("server name is required");
        }
        let url = Url::parse(self.url.trim()).context("--url must be a valid URL")?;
        if !matches!(url.scheme(), "http" | "https")
            || url.host_str().is_none()
            || !url.username().is_empty()
            || url.password().is_some()
            || url.query().is_some()
            || url.fragment().is_some()
        {
            bail!("--url must be an HTTP(S) base URL without credentials, query, or fragment");
        }
        if self.gcp_instance.is_empty() != self.gcp_zone.is_empty() {
            bail!("--gcp-instance and --gcp-zone must be set together");
        }
        if !self.tenant_key.is_empty() && !tenant::valid_key_format(&self.tenant_key) {
            bail!("--tenant-key must look like srt_<32 hex>");
        }
        Ok(())
    }

    #[must_use]
    pub fn proxy_root(&self) -> String {
        let root = proxy_root(&self.url);
        if self.tenant_key.is_empty() {
            root
        } else {
            format!("{root}/t/{}", self.tenant_key)
        }
    }

    #[must_use]
    pub fn codex_base_url(&self) -> String {
        format!("{}/v1", self.proxy_root())
    }

    #[must_use]
    pub fn control_base_url(&self) -> String {
        self.proxy_root()
    }
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct ServerFile {
    #[serde(default)]
    pub servers: Vec<ServerConfig>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub default: String,
}

#[derive(Clone, Debug)]
pub struct Store {
    path: PathBuf,
}

impl Default for Store {
    fn default() -> Self {
        Self::new(CodexStore::default().store_dir().join("servers.json"))
    }
}

impl Store {
    #[must_use]
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self { path: path.into() }
    }

    #[must_use]
    pub fn path(&self) -> &Path {
        &self.path
    }

    pub fn load(&self) -> anyhow::Result<ServerFile> {
        let body = match fs::read(&self.path) {
            Ok(body) => body,
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                return Ok(ServerFile::default());
            }
            Err(error) => return Err(error.into()),
        };
        let mut file: ServerFile = serde_json::from_slice(&body)?;
        file.servers
            .sort_by(|left, right| left.name.cmp(&right.name));
        Ok(file)
    }

    pub fn save(&self, mut file: ServerFile) -> anyhow::Result<()> {
        file.servers
            .sort_by(|left, right| left.name.cmp(&right.name));
        let mut body = serde_json::to_vec_pretty(&file)?;
        body.push(b'\n');
        write_atomic_private(&self.path, &body)?;
        Ok(())
    }

    pub fn find(&self, name: &str) -> anyhow::Result<Option<ServerConfig>> {
        Ok(self
            .load()?
            .servers
            .into_iter()
            .find(|server| server.name == name))
    }

    pub fn upsert(
        &self,
        mut server: ServerConfig,
        make_default: bool,
        preserve_admin_token: bool,
        preserve_tenant_key: bool,
    ) -> anyhow::Result<ServerFile> {
        server.name = server.name.trim().into();
        server.url = server.url.trim().trim_end_matches('/').into();
        server.tenant_key = server.tenant_key.trim().into();
        server.validate()?;
        let mut file = self.load()?;
        if let Some(existing) = file
            .servers
            .iter_mut()
            .find(|existing| existing.name == server.name)
        {
            if preserve_admin_token {
                server.admin_token.clone_from(&existing.admin_token);
            }
            if preserve_tenant_key {
                server.tenant_key.clone_from(&existing.tenant_key);
            }
            *existing = server.clone();
        } else {
            file.servers.push(server.clone());
        }
        if make_default {
            file.default.clone_from(&server.name);
        }
        self.save(file.clone())?;
        Ok(file)
    }

    pub fn select(&self, name: Option<&str>) -> anyhow::Result<Option<ServerConfig>> {
        let explicit = name.map(str::to_owned).or_else(|| {
            std::env::var("SUBROUTER_CODEX_SERVER")
                .ok()
                .filter(|value| !value.trim().is_empty())
        });
        if explicit
            .as_deref()
            .is_some_and(|value| value.eq_ignore_ascii_case("local"))
        {
            return Ok(None);
        }
        let file = self.load()?;
        let selected = explicit.unwrap_or(file.default);
        if selected.is_empty() {
            return Ok(None);
        }
        file.servers
            .into_iter()
            .find(|server| server.name == selected)
            .map(Some)
            .ok_or_else(|| anyhow!("configured Subrouter server {selected:?} was not found"))
    }

    pub fn use_server(&self, name: Option<&str>) -> anyhow::Result<Option<ServerConfig>> {
        let mut file = self.load()?;
        let server = match name {
            None | Some("local") => {
                file.default.clear();
                None
            }
            Some(name) => {
                let server = file
                    .servers
                    .iter()
                    .find(|server| server.name == name)
                    .cloned()
                    .ok_or_else(|| anyhow!("server {name:?} not found"))?;
                file.default = name.into();
                Some(server)
            }
        };
        self.save(file)?;
        Ok(server)
    }

    pub fn remove(&self, name: &str) -> anyhow::Result<bool> {
        let mut file = self.load()?;
        let before = file.servers.len();
        file.servers.retain(|server| server.name != name);
        if file.default == name {
            file.default.clear();
        }
        let changed = file.servers.len() != before;
        if changed {
            self.save(file)?;
        }
        Ok(changed)
    }

    pub fn rename(&self, old: &str, new: &str) -> anyhow::Result<()> {
        if new.trim().is_empty() {
            bail!("new server name is required");
        }
        let mut file = self.load()?;
        if file.servers.iter().any(|server| server.name == new) {
            bail!("server {new:?} already exists");
        }
        let server = file
            .servers
            .iter_mut()
            .find(|server| server.name == old)
            .ok_or_else(|| anyhow!("server {old:?} not found"))?;
        server.name = new.into();
        if file.default == old {
            file.default = new.into();
        }
        self.save(file)
    }
}

#[must_use]
pub fn proxy_root(value: &str) -> String {
    let mut root = value.trim().trim_end_matches('/').to_owned();
    for suffix in ["/v1", "/backend-api"] {
        if root.ends_with(suffix) {
            root.truncate(root.len() - suffix.len());
            break;
        }
    }
    root.trim_end_matches('/').into()
}

pub fn write_codex_config(server: Option<&ServerConfig>) -> anyhow::Result<PathBuf> {
    let root = server.map_or_else(|| LOCAL_BASE_URL.into(), ServerConfig::proxy_root);
    let path = default_codex_config_path()?;
    let body = match fs::read_to_string(&path) {
        Ok(body) => body,
        Err(error) if error.kind() == io::ErrorKind::NotFound => String::new(),
        Err(error) => return Err(error.into()),
    };
    let mut document = if body.trim().is_empty() {
        DocumentMut::new()
    } else {
        body.parse::<DocumentMut>()
            .with_context(|| format!("parse {}", path.display()))?
    };
    document["openai_base_url"] = value(format!("{root}/v1"));
    document["chatgpt_base_url"] = value(format!("{root}/backend-api"));
    document["experimental_realtime_ws_base_url"] = value(format!("{root}/v1"));
    let next = document.to_string();
    if next == body {
        return Ok(path);
    }
    if !body.is_empty() {
        write_atomic_private(&path.with_extension("toml.bak"), body.as_bytes())
            .context("write Codex config backup")?;
    }
    write_atomic_private(&path, next.as_bytes())?;
    Ok(path)
}

pub fn default_codex_config_path() -> anyhow::Result<PathBuf> {
    if let Some(home) =
        std::env::var_os("CODEX_HOME").filter(|value| !value.to_string_lossy().trim().is_empty())
    {
        return Ok(PathBuf::from(home).join("config.toml"));
    }
    let home = std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
        .ok_or_else(|| anyhow!("home directory is unavailable"))?;
    Ok(PathBuf::from(home).join(".codex/config.toml"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn store_is_go_compatible_and_redacts_debug() {
        let root = tempfile::tempdir().unwrap();
        let store = Store::new(root.path().join("servers.json"));
        store
            .upsert(
                ServerConfig {
                    name: "team".into(),
                    url: "https://example.com/".into(),
                    admin_token: "secret-admin".into(),
                    tenant_key: "srt_00000000000000000000000000000000".into(),
                    ..ServerConfig::default()
                },
                true,
                false,
                false,
            )
            .unwrap();
        let selected = store.select(None).unwrap().unwrap();
        assert_eq!(
            selected.codex_base_url(),
            "https://example.com/t/srt_00000000000000000000000000000000/v1"
        );
        let raw = fs::read_to_string(store.path()).unwrap();
        assert!(raw.contains(r#""adminToken": "secret-admin""#));
        assert!(!format!("{selected:?}").contains("secret-admin"));
    }

    #[test]
    fn proxy_root_strips_known_codex_suffixes() {
        assert_eq!(
            proxy_root("http://localhost:31415/v1/"),
            "http://localhost:31415"
        );
        assert_eq!(
            proxy_root("http://localhost:31415/backend-api"),
            "http://localhost:31415"
        );
    }
}
