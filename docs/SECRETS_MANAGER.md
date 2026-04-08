# Managing Curator API Keys with AWS Secrets Manager

## Overview

API keys for LFS upload authorization are stored in AWS Secrets Manager as a
JSON object mapping curator names to their keys. The Lambda function fetches
this secret at startup and caches it for 5 minutes, so changes take effect
within 5 minutes without requiring redeployment.

The secret format is:

```json
{
  "alice": "key1",
  "bob": "key2",
  "external-curator": "key3"
}
```

Each curator name appears in CloudWatch logs when they push, providing a full
audit trail of who uploaded what and when.

---

## Full Flow

### Upload (curator pushing LFS objects)

```
git lfs push
    │
    ├─► GitHub
    │     └─► receives pointer files (tiny text files), normal git push
    │
    └─► lfs-broker  (POST /objects/batch, Authorization: Basic ...)
            │
            └─► Secrets Manager
                    └─► fetches key map by LFS_SECRET_NAME ("github-lfs-api-keys")
                            └─► {"alice":"key1","bob":"key2",...}
                                    │
                                    ├─► key matches: log "authorized upload by alice"
                                    │       └─► return pre-signed S3 PUT URL
                                    │               └─► git-lfs uploads file directly to S3
                                    │
                                    └─► no match: return 401 Unauthorized
```

### Download (researcher pulling LFS objects)

```
git lfs pull
    │
    ├─► GitHub
    │     └─► provides pointer files
    │
    └─► lfs-broker  (POST /objects/batch, no credentials required)
            │
            └─► return pre-signed S3 GET URL
                    └─► git-lfs downloads file directly from S3
```

---

## How Authentication Works

When a curator runs `git lfs push`, the git-lfs client sends an HTTP request
directly to the broker (not to GitHub) with the curator's API key in the
`Authorization` header. GitHub is not involved in this step — the `.lfsconfig`
file redirects LFS traffic away from GitHub to the broker URL.

The broker authenticates the request as follows:

1. Reads the `LFS_SECRET_NAME` environment variable to get the name of the
   Secrets Manager secret (e.g. `github-lfs-api-keys`)
2. Fetches the secret from Secrets Manager — a JSON object mapping curator
   names to their keys
3. Compares the key from the `Authorization` header against every value in
   that map
4. If a match is found, the curator's name is logged and a pre-signed S3 PUT
   URL is returned; otherwise the request is rejected with 401

The keys themselves never live in the broker's environment — only the secret
name does. This means keys can be added, revoked, or rotated in Secrets
Manager without redeploying the broker. Changes take effect within 5 minutes
(the broker's cache TTL).

---

## IAM Role Update

This section shows the inline policy required for the Lambda to read from
Secrets Manager. It is provided here for reference only — the canonical
source is `docs/inline-policy.json` and the role setup is covered in
`docs/AWS_SETUP.md`. If you have already followed `AWS_SETUP.md` you do
not need to make any changes here.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject"
      ],
      "Resource": "arn:aws:s3:::YOUR_BUCKET_NAME/*"
    },
    {
      "Effect": "Allow",
      "Action": "secretsmanager:GetSecretValue",
      "Resource": "arn:aws:secretsmanager:us-east-1:YOUR_ACCOUNT_ID:secret:github-lfs-api-keys*"
    }
  ]
}
```

Note the `*` wildcard at the end of the secret ARN — Secrets Manager appends
a random suffix to secret ARNs which makes exact ARN matching impractical.

---

## Lambda Environment Variable Update

The Lambda now uses `LFS_SECRET_NAME` instead of `LFS_API_KEY`:

```bash
aws lambda update-function-configuration \
  --function-name github-lfs \
  --environment Variables="{S3_BUCKET=YOUR_BUCKET_NAME,LFS_SECRET_NAME=github-lfs-api-keys}" \
  --region us-east-1 \
  --profile YOUR_PROFILE_NAME
```

---

## Secret Management Commands

### Create the Secret (first time only)

```bash
aws secretsmanager create-secret \
  --name github-lfs-api-keys \
  --description "API keys for cBioPortal datahub LFS upload authorization" \
  --secret-string '{"alice":"REPLACE_WITH_KEY","bob":"REPLACE_WITH_KEY"}' \
  --region us-east-1 \
  --profile YOUR_PROFILE_NAME
```

Generate a key for each curator:

```bash
openssl rand -hex 32
```

### View Current Keys

```bash
aws secretsmanager get-secret-value \
  --secret-id github-lfs-api-keys \
  --region us-east-1 \
  --query SecretString \
  --output text \
  --profile YOUR_PROFILE_NAME
```

### Add a New Curator

First retrieve the current secret, then add the new curator:

```bash
# Get current value
CURRENT=$(aws secretsmanager get-secret-value \
  --secret-id github-lfs-api-keys \
  --region us-east-1 \
  --query SecretString \
  --output text \
  --profile YOUR_PROFILE_NAME)

# Generate a new key
NEW_KEY=$(openssl rand -hex 32)

# Add new curator (requires jq)
UPDATED=$(echo $CURRENT | jq --arg name "new-curator" --arg key "$NEW_KEY" '. + {($name): $key}')

# Update the secret
aws secretsmanager update-secret \
  --secret-id github-lfs-api-keys \
  --secret-string "$UPDATED" \
  --region us-east-1 \
  --profile YOUR_PROFILE_NAME

# Print the new key to distribute to the curator
echo "New key for new-curator: $NEW_KEY"
```

### Revoke a Curator's Key

```bash
# Get current value
CURRENT=$(aws secretsmanager get-secret-value \
  --secret-id github-lfs-api-keys \
  --region us-east-1 \
  --query SecretString \
  --output text \
  --profile YOUR_PROFILE_NAME)

# Remove the curator (requires jq)
UPDATED=$(echo $CURRENT | jq 'del(.["curator-name"])')

# Update the secret
aws secretsmanager update-secret \
  --secret-id github-lfs-api-keys \
  --secret-string "$UPDATED" \
  --region us-east-1 \
  --profile YOUR_PROFILE_NAME
```

Revocation takes effect within 5 minutes (the Lambda cache TTL).

### Rotate a Curator's Key

```bash
# Generate a new key
NEW_KEY=$(openssl rand -hex 32)

# Get current value
CURRENT=$(aws secretsmanager get-secret-value \
  --secret-id github-lfs-api-keys \
  --region us-east-1 \
  --query SecretString \
  --output text \
  --profile YOUR_PROFILE_NAME)

# Replace the key for the curator (requires jq)
UPDATED=$(echo $CURRENT | jq --arg name "curator-name" --arg key "$NEW_KEY" '.[$name] = $key')

# Update the secret
aws secretsmanager update-secret \
  --secret-id github-lfs-api-keys \
  --secret-string "$UPDATED" \
  --region us-east-1 \
  --profile YOUR_PROFILE_NAME

# Print the new key to distribute to the curator
echo "New key for curator-name: $NEW_KEY"
```

After rotation, the curator must update their stored credential:

```bash
git credential approve <<EOF
protocol=https
host=<your-function-url>.lambda-url.us-east-1.on.aws
username=lfs
password=NEW_KEY
EOF
```

---

## Audit Trail

Every upload attempt is logged to CloudWatch with the curator name:

```
authorized upload by curator: alice
generated upload URL for oid abc123...
```

Unauthorized attempts are also logged:

```
unauthorized upload attempt
```

To query logs for a specific curator's activity:

```bash
aws logs filter-log-events \
  --log-group-name /aws/lambda/github-lfs \
  --filter-pattern "alice" \
  --region us-east-1 \
  --profile YOUR_PROFILE_NAME
```

To see all upload activity:

```bash
aws logs filter-log-events \
  --log-group-name /aws/lambda/github-lfs \
  --filter-pattern "authorized upload by curator" \
  --region us-east-1 \
  --profile YOUR_PROFILE_NAME
```
