use std::fmt;
use std::str::FromStr;

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

#[derive(
    Clone, Copy, Debug, Default, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize,
)]
#[serde(rename_all = "lowercase")]
pub enum Provider {
    #[default]
    Codex,
    Claude,
    Kimi,
    Zai,
}

impl Provider {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Codex => "codex",
            Self::Claude => "claude",
            Self::Kimi => "kimi",
            Self::Zai => "zai",
        }
    }
}

impl fmt::Display for Provider {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

impl FromStr for Provider {
    type Err = String;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value.trim().to_ascii_lowercase().as_str() {
            "codex" => Ok(Self::Codex),
            "claude" => Ok(Self::Claude),
            "kimi" => Ok(Self::Kimi),
            "zai" => Ok(Self::Zai),
            value => Err(format!("unknown provider {value:?}")),
        }
    }
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, Hash, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum AuthMode {
    #[default]
    Oauth,
    #[serde(rename = "apikey")]
    ApiKey,
}

#[derive(Clone, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub struct Account {
    pub id: String,
    pub provider: Provider,
    pub auth_mode: AuthMode,
    pub label: String,
    pub email: String,
    pub added_at: Option<DateTime<Utc>>,
    pub token: String,
    pub account_id: String,
    pub source: String,
}

impl Account {
    #[must_use]
    pub fn authorization_header(&self) -> String {
        if self.token.is_empty() {
            String::new()
        } else {
            format!("Bearer {}", self.token)
        }
    }
}

impl fmt::Debug for Account {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Account")
            .field("id", &self.id)
            .field("provider", &self.provider)
            .field("auth_mode", &self.auth_mode)
            .field("label", &self.label)
            .field("email", &self.email)
            .field("added_at", &self.added_at)
            .field(
                "token",
                &if self.token.is_empty() {
                    "<empty>"
                } else {
                    "<redacted>"
                },
            )
            .field("account_id", &self.account_id)
            .field("source", &self.source)
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn authorization_header_is_bearer_or_empty() {
        let mut account = Account::default();
        assert_eq!(account.authorization_header(), "");
        account.token = "secret".into();
        assert_eq!(account.authorization_header(), "Bearer secret");
        assert!(!format!("{account:?}").contains("secret"));
    }
}
