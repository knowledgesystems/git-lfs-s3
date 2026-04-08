# docs

This directory contains documentation and supporting files for setting up
and operating the git-lfs-s3 infrastructure.

---

## Documents

### [AWS_SETUP.md](AWS_SETUP.md)

The primary setup guide. Covers all AWS infrastructure required to deploy
git-lfs-s3, including prerequisites, authentication, resource creation, and
deployment order. Start here if you are setting up the infrastructure for
the first time.

### [SECRETS_MANAGER.md](SECRETS_MANAGER.md)

Instructions for managing curator API keys in AWS Secrets Manager. Covers
creating the secret, adding and revoking individual curator keys, rotating
keys, and querying the CloudWatch audit trail.

### [BACKFILL.md](BACKFILL.md)

Instructions for migrating existing LFS objects from GitHub to S3. Covers
both full and incremental migration approaches, verification, and switching
the repository to the S3-backed broker once migration is complete.

---

## Policy Files

### [trust-policy.json](trust-policy.json)

IAM trust policy for the `github-lfs-lambda-role` role. Defines that the
AWS Lambda service is allowed to assume this role. Used with:

```bash
aws iam create-role \
  --role-name github-lfs-lambda-role \
  --assume-role-policy-document file://docs/trust-policy.json
```

### [inline-policy.json](inline-policy.json)

IAM inline policy for the `github-lfs-lambda-role` role. Grants the Lambda
function permission to read and write objects in the S3 bucket and to read
curator API keys from Secrets Manager. Replace `YOUR_BUCKET_NAME` and
`YOUR_ACCOUNT_ID` before use. Used with:

```bash
aws iam put-role-policy \
  --role-name github-lfs-lambda-role \
  --policy-name github-lfs-s3-access \
  --policy-document file://docs/inline-policy.json
```

### [bucket-policy.json](bucket-policy.json)

S3 bucket policy that controls access within the single shared bucket.
Makes snapshot files under `public/*` publicly readable while restricting
`lfs/*` to the Lambda role only. Pre-signed URLs for LFS downloads still
work because they are signed with the Lambda role's credentials. Replace
`YOUR_BUCKET_NAME` and `YOUR_ACCOUNT_ID` before use. Used with:

```bash
aws s3api put-bucket-policy \
  --bucket YOUR_BUCKET_NAME \
  --policy file://docs/bucket-policy.json
```

### [cross-account-bucket-policy.json](cross-account-bucket-policy.json)

S3 bucket policy applied to a bucket in a second AWS account to allow the
Lambda role from the first account to write snapshot files to it. Used with
the `EXTRA_SNAPSHOT_BUCKETS` environment variable. Replace `FIRST_ACCOUNT_ID`
and `SECOND_ACCOUNT_BUCKET` before use. Used with:

```bash
aws s3api put-bucket-policy \
  --bucket SECOND_ACCOUNT_BUCKET \
  --policy file://docs/cross-account-bucket-policy.json \
  --profile SECOND_ACCOUNT_PROFILE
```

---

## GitHub Action

### [snapshot.yml](snapshot.yml)

GitHub Actions workflow that triggers the lfs-snapshot Lambda on every push
to master. Copy this file into the target repository at
`.github/workflows/snapshot.yml`. Requires a `SNAPSHOT_LAMBDA_URL` secret
to be set in the target repository's GitHub settings.

---

## Policy Relationships

The policy files work together but serve distinct purposes:

| File | Attached to | Purpose |
|---|---|---|
| `trust-policy.json` | IAM role | Who can assume the role |
| `inline-policy.json` | IAM role | What the role is allowed to do |
| `bucket-policy.json` | S3 bucket (primary) | Who is allowed to access the primary bucket |
| `cross-account-bucket-policy.json` | S3 bucket (second account) | Grants Lambda role write access from first account |
