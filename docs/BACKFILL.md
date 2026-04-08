# Backfilling the S3 LFS Bucket

This document describes how to migrate existing LFS objects from GitHub to
your S3 LFS bucket. This is a one-time operation performed before switching
a repository from GitHub LFS to the S3-backed Lambda broker.

---

## Prerequisites

### Disk Space

LFS objects for large repositories can be substantial. Check how many LFS
objects exist before starting:

```bash
git lfs ls-files --all | wc -l
```

Make sure you have enough local disk space to hold all LFS objects during
the migration.

### AWS CLI

Make sure you are authenticated and can access the target S3 bucket:

```bash
aws s3 ls s3://YOUR_LFS_BUCKET/ --profile YOUR_PROFILE_NAME
```

---

## Full Backfill

Use this approach to migrate all LFS objects from a repository at once.

### 1. Clone the Repository

Clone without checking out files to avoid downloading LFS objects twice:

```bash
git clone --no-checkout https://github.com/YOUR_ORG/YOUR_REPO.git
cd YOUR_REPO
git lfs install --skip-smudge
git checkout master
```

### 2. Pull All LFS Objects

```bash
git lfs pull
```

This downloads all LFS objects into `.git/lfs/objects/` using the standard
content-addressable layout that matches the S3 bucket structure.

### 3. Sync to S3

```bash
aws s3 sync .git/lfs/objects/ s3://YOUR_LFS_BUCKET/lfs/objects/ \
  --profile YOUR_PROFILE_NAME
```

`aws s3 sync` only copies files that don't already exist in S3, so it is
safe to run multiple times and will resume if interrupted.

### 4. Verify

Confirm the object count matches between local and S3:

```bash
# Local count
find .git/lfs/objects -type f | wc -l

# S3 count
aws s3 ls s3://YOUR_LFS_BUCKET/lfs/objects/ --recursive \
  --profile YOUR_PROFILE_NAME | wc -l
```

Both numbers should match.

---

## Incremental Backfill

For large repositories where a full pull is impractical due to disk space
or time constraints, you can backfill one study or folder at a time.

### 1. Clone the Repository

```bash
git clone --no-checkout https://github.com/YOUR_ORG/YOUR_REPO.git
cd YOUR_REPO
git lfs install --skip-smudge
git checkout master
```

### 2. Pull and Sync One Folder at a Time

```bash
# Pull LFS objects for a specific folder
git lfs pull -I public/brca_tcga

# Sync to S3
aws s3 sync .git/lfs/objects/ s3://YOUR_LFS_BUCKET/lfs/objects/ \
  --profile YOUR_PROFILE_NAME

# Pull the next folder
git lfs pull -I public/msk_impact_2017

# Sync again (only new objects are uploaded)
aws s3 sync .git/lfs/objects/ s3://YOUR_LFS_BUCKET/lfs/objects/ \
  --profile YOUR_PROFILE_NAME
```

Repeat for each folder. The `aws s3 sync` command is idempotent so running
it after each pull is safe — it will only upload objects that aren't already
in S3.

---

## Switching the Repository to S3-Backed LFS

Once all LFS objects are in S3, add `.lfsconfig` to the repository to
redirect LFS traffic to the Lambda broker:

```bash
echo '[lfs]' > .lfsconfig
echo '    url = https://YOUR_FUNCTION_URL.lambda-url.us-east-1.on.aws/' >> .lfsconfig

git add .lfsconfig
git commit -m "redirect LFS storage to S3-backed Lambda"
git push
```

Get your Function URL with:

```bash
make url-broker
```

---

## Cleaning Up GitHub LFS Storage

After confirming the migration is complete and the Lambda broker is working
correctly, you can contact GitHub support to purge the old LFS objects from
GitHub's storage to reclaim the quota.

Before doing so, verify the broker is fully operational:

```bash
# Test a download via the Lambda
curl -X POST https://YOUR_FUNCTION_URL.lambda-url.us-east-1.on.aws/ \
  -H "Content-Type: application/json" \
  -d '{"operation":"download","objects":[{"oid":"SAMPLE_OID","size":100}]}'
```

You should receive a valid pre-signed S3 URL in the response confirming
the broker can serve objects from S3.

---

## For datahub

The specific commands for migrating cBioPortal datahub:

```bash
# Clone
git clone --no-checkout https://github.com/cBioPortal/datahub.git
cd datahub
git lfs install --skip-smudge
git checkout master

# Pull all LFS objects (this may take a while)
git lfs pull

# Sync to S3
aws s3 sync .git/lfs/objects/ s3://YOUR_BUCKET_NAME/lfs/objects/ \
  --profile YOUR_PROFILE_NAME

# Verify
find .git/lfs/objects -type f | wc -l
aws s3 ls s3://YOUR_BUCKET_NAME/lfs/objects/ --recursive \
  --profile YOUR_PROFILE_NAME | wc -l
```
