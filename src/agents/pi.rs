use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::bail;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::accounts::{StoredCodexAccount, extract_chatgpt_account_id, jwt_expiry_millis};

use super::opencode::{home_dir, write_atomic_private};

#[derive(Clone, Default, Deserialize, Serialize)]
pub struct Entry {
    #[serde(rename = "type")]
    pub entry_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub key: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub refresh: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub access: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub expires: i64,
    #[serde(
        rename = "accountId",
        default,
        skip_serializing_if = "String::is_empty"
    )]
    pub account_id: String,
}

const fn is_zero(value: &i64) -> bool {
    *value == 0
}

#[must_use]
pub fn default_auth_path() -> PathBuf {
    let dir = std::env::var_os("PI_CODING_AGENT_DIR")
        .filter(|value| !value.to_string_lossy().is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            home_dir()
                .unwrap_or_else(|| PathBuf::from("."))
                .join(".pi/agent")
        });
    expand_tilde(&dir).join("auth.json")
}

pub fn entry_for_codex_account(
    account: &StoredCodexAccount,
) -> anyhow::Result<(&'static str, Entry)> {
    if account.is_api_key() {
        if account.auth.openai_api_key.is_empty() {
            bail!("pi sync requires a non-empty OpenAI API key");
        }
        return Ok((
            "openai",
            Entry {
                entry_type: "api_key".into(),
                key: account.auth.openai_api_key.clone(),
                ..Entry::default()
            },
        ));
    }
    let Some(tokens) = account
        .auth
        .tokens
        .as_ref()
        .filter(|tokens| !tokens.access_token.is_empty() && !tokens.refresh_token.is_empty())
    else {
        bail!("pi sync requires Codex OAuth tokens");
    };
    let expires = jwt_expiry_millis(&tokens.access_token).unwrap_or_else(|| {
        (SystemTime::now() + Duration::from_secs(60 * 60))
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as i64
    });
    Ok((
        "openai-codex",
        Entry {
            entry_type: "oauth".into(),
            refresh: tokens.refresh_token.clone(),
            access: tokens.access_token.clone(),
            expires,
            account_id: extract_chatgpt_account_id(&account.auth),
            ..Entry::default()
        },
    ))
}

pub fn sync_codex_account(account: &StoredCodexAccount) -> anyhow::Result<PathBuf> {
    let (provider, entry) = entry_for_codex_account(account)?;
    let path = default_auth_path();
    let mut data: BTreeMap<String, Value> = match fs::read(&path) {
        Ok(body) => serde_json::from_slice(&body)
            .map_err(|error| anyhow::anyhow!("parse {}: {error}", path.display()))?,
        Err(error) if error.kind() == io::ErrorKind::NotFound => BTreeMap::new(),
        Err(error) => return Err(error.into()),
    };
    data.insert(provider.into(), serde_json::to_value(entry)?);
    let mut body = serde_json::to_vec_pretty(&data)?;
    body.push(b'\n');
    write_atomic_private(&path, &body)?;
    Ok(path)
}

fn expand_tilde(path: &Path) -> PathBuf {
    let text = path.to_string_lossy();
    if text == "~" {
        return home_dir().unwrap_or_else(|| path.to_path_buf());
    }
    if let Some(rest) = text.strip_prefix("~/")
        && let Some(home) = home_dir()
    {
        return home.join(rest);
    }
    path.to_path_buf()
}
