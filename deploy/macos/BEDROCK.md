# Serving Claude Fable from AWS Bedrock

The team subrouter can serve Claude Fable (`claude-fable-5`) from AWS Bedrock so
Fable never hard-fails when the Claude subscription pool is rate-limited. The
subrouter signs each request with AWS SigV4 and forwards it to
`bedrock-runtime.<region>.amazonaws.com`; clients never hold AWS credentials.

Fable routing order is subscription pool first, then Bedrock, then a dedicated
Anthropic API key. Set `--fable-bedrock-primary` to send Fable to Bedrock first
instead (see below).

## Use a SigV4 IAM key, not a Bedrock API key

AWS Bedrock API keys (`ABSK...`, sent as a bearer/`x-api-key` to the Mantle
endpoint) are bound to your organization's `default` project, whose data
retention mode is `default`. Fable rejects that with:

```
data retention mode 'default' is not available for this model
```

Fixing it requires setting the project to `provider_data_share`, but the API
returns `cannot update the default project`, and a new project's key cannot be
minted through the public API. A plain SigV4 IAM key sidesteps the project
entirely and invokes Fable directly, so the subrouter uses SigV4. Do not use a
Bedrock API key here.

## 1. Create a scoped IAM key

With admin credentials for the Bedrock account:

```bash
aws iam create-user --user-name subrouter-bedrock-fable
aws iam put-user-policy --user-name subrouter-bedrock-fable \
  --policy-name invoke-fable --policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
      "Resource": [
        "arn:aws:bedrock:*::foundation-model/anthropic.claude-fable-5*",
        "arn:aws:bedrock:*:<ACCOUNT_ID>:inference-profile/us.anthropic.claude-fable-5*"
      ]
    }]
  }'
aws iam create-access-key --user-name subrouter-bedrock-fable
```

Confirm the key works before installing (allow a few seconds for IAM to
propagate):

```bash
curl -sS -X POST \
  "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-fable-5/invoke" \
  --aws-sigv4 "aws:amz:us-east-1:bedrock" --user "$AKID:$SECRET" \
  -H "Content-Type: application/json" \
  -d '{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
# -> 200 with a claude-fable-5 message
```

## 2. Install with Bedrock enabled

Pass the key to `install-macos.sh` through the environment. Pick a random
gateway token; clients must present it to the external `/bedrock/*` route (the
internal Fable fallback does not need it).

```bash
sudo SUBROUTER_ENABLE_BEDROCK=1 \
  SUBROUTER_BEDROCK_AWS_ACCESS_KEY_ID=AKIA... \
  SUBROUTER_BEDROCK_AWS_SECRET_ACCESS_KEY=... \
  SUBROUTER_BEDROCK_REGION=us-east-1 \
  SUBROUTER_BEDROCK_GATEWAY_TOKEN="$(openssl rand -hex 16)" \
  SUBROUTER_FABLE_BEDROCK_PRIMARY=1 \
  ./install-macos.sh
```

This writes the AWS profile to `/var/lib/subrouter/.aws` (owned by `_subrouter`,
`0600`), adds the `--bedrock` flags to the team LaunchDaemon, and restarts it.
Drop `SUBROUTER_FABLE_BEDROCK_PRIMARY=1` to keep Bedrock as a fallback instead of
the primary Fable route.

Env vars:

| Variable | Default | Meaning |
| --- | --- | --- |
| `SUBROUTER_ENABLE_BEDROCK` | `0` | `1` turns on the Bedrock gateway. |
| `SUBROUTER_BEDROCK_AWS_ACCESS_KEY_ID` | — | SigV4 access key id (required). |
| `SUBROUTER_BEDROCK_AWS_SECRET_ACCESS_KEY` | — | SigV4 secret (required). |
| `SUBROUTER_BEDROCK_REGION` | `us-east-1` | Bedrock region for Fable. |
| `SUBROUTER_BEDROCK_PROFILE` | `aw1` | AWS profile name written under `.aws`. |
| `SUBROUTER_BEDROCK_GATEWAY_TOKEN` | — | Bearer token for the external `/bedrock/*` route. |
| `SUBROUTER_FABLE_BEDROCK_PRIMARY` | `0` | `1` routes Fable to Bedrock before the pool. |

## 3. Verify

```bash
# gateway enabled on startup
sudo grep "bedrock gateway enabled" /var/log/subrouter.err.log | tail -1

# direct invoke through the gateway (needs the token)
curl -sS -X POST http://127.0.0.1:31415/bedrock/model/us.anthropic.claude-fable-5/invoke \
  -H "Authorization: Bearer $SUBROUTER_BEDROCK_GATEWAY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
# -> 200; without the token -> 401

# Fable through the proxy (id is msg_bdrk_... when served by Bedrock)
curl -sS -X POST http://127.0.0.1:31415/v1/messages \
  -H "X-Subrouter-Agent: claude" -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-fable-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'

# tracked spend
curl -sS http://127.0.0.1:31415/_subrouter/bedrock-cost
```

## Notes

- Bedrock Fable is paid; the pool-first default drains free subscription quota
  before paying. `--fable-bedrock-primary` pays Bedrock for every Fable token.
- The config survives daemon restart, reboot, and auto-update. Re-running
  `install-macos.sh` re-applies it only if the `SUBROUTER_ENABLE_BEDROCK` env is
  set again.
- Rotate the IAM key with `aws iam create-access-key` / `delete-access-key` and
  re-run the install with the new value.
