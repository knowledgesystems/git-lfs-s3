REGION                  = us-east-1
BROKER_NAME             = github-lfs
SNAPSHOT_NAME           = github-lfs-snapshot
PROFILE                 = YOUR_PROFILE_NAME
LFS_BUCKET              = YOUR_BUCKET_NAME
SNAPSHOT_BUCKET         = YOUR_BUCKET_NAME
SNAPSHOT_PREFIX         = public/
EXTRA_SNAPSHOT_BUCKETS  =
ACCOUNT_ID              = YOUR_ACCOUNT_ID
ROLE_NAME               = github-lfs-lambda-role
REPO                    = YOUR_ORG/YOUR_REPO
REF                     = master
GITHUB_HOST             = github.com

# -----------------------------------------------------------------------
# Init (first time only - sets up Go modules)
# -----------------------------------------------------------------------

init-broker:
	cd lfs-broker && go mod init github.com/cbioportal/lfs-broker && \
	go get github.com/aws/aws-lambda-go@latest && \
	go get github.com/aws/aws-sdk-go-v2@latest && \
	go get github.com/aws/aws-sdk-go-v2/config@latest && \
	go get github.com/aws/aws-sdk-go-v2/service/s3@latest && \
	go get github.com/aws/aws-sdk-go-v2/service/secretsmanager@latest && \
	go mod tidy

init-snapshot:
	cd lfs-snapshot && go mod init github.com/cbioportal/lfs-snapshot && \
	go get github.com/aws/aws-lambda-go@latest && \
	go get github.com/aws/aws-sdk-go-v2@latest && \
	go get github.com/aws/aws-sdk-go-v2/config@latest && \
	go get github.com/aws/aws-sdk-go-v2/service/s3@latest && \
	go mod tidy

init: init-broker init-snapshot

# -----------------------------------------------------------------------
# Bucket setup
# -----------------------------------------------------------------------

configure-bucket:
	aws s3api put-public-access-block \
		--bucket $(LFS_BUCKET) \
		--public-access-block-configuration \
		"BlockPublicAcls=false,IgnorePublicAcls=false,BlockPublicPolicy=false,RestrictPublicBuckets=false" \
		--region $(REGION) \
		--profile $(PROFILE)
	aws s3api put-bucket-policy \
		--bucket $(LFS_BUCKET) \
		--policy file://docs/bucket-policy.json \
		--region $(REGION) \
		--profile $(PROFILE)

# -----------------------------------------------------------------------
# Build
# -----------------------------------------------------------------------

build-broker:
	cd lfs-broker && GOOS=linux GOARCH=amd64 go build -o bootstrap main.go && zip function.zip bootstrap

build-snapshot:
	cd lfs-snapshot && GOOS=linux GOARCH=amd64 go build -o bootstrap main.go && zip function.zip bootstrap

build: build-broker build-snapshot

# -----------------------------------------------------------------------
# Create (first time setup)
# -----------------------------------------------------------------------

create-broker: build-broker
	aws lambda create-function \
		--function-name $(BROKER_NAME) \
		--runtime provided.al2023 \
		--role arn:aws:iam::$(ACCOUNT_ID):role/$(ROLE_NAME) \
		--handler bootstrap \
		--zip-file fileb://lfs-broker/function.zip \
		--environment Variables="{S3_BUCKET=$(LFS_BUCKET),LFS_SECRET_NAME=github-lfs-api-keys}" \
		--timeout 30 \
		--region $(REGION) \
		--profile $(PROFILE)
	aws lambda create-function-url-config \
		--function-name $(BROKER_NAME) \
		--auth-type NONE \
		--region $(REGION) \
		--profile $(PROFILE)
	aws lambda add-permission \
		--function-name $(BROKER_NAME) \
		--statement-id FunctionURLAllowPublicAccess \
		--action lambda:InvokeFunctionUrl \
		--principal "*" \
		--function-url-auth-type NONE \
		--region $(REGION) \
		--profile $(PROFILE)
	aws lambda add-permission \
		--function-name $(BROKER_NAME) \
		--statement-id FunctionURLAllowInvoke \
		--action lambda:InvokeFunction \
		--principal "*" \
		--region $(REGION) \
		--profile $(PROFILE)

create-snapshot: build-snapshot
	aws lambda create-function \
		--function-name $(SNAPSHOT_NAME) \
		--runtime provided.al2023 \
		--role arn:aws:iam::$(ACCOUNT_ID):role/$(ROLE_NAME) \
		--handler bootstrap \
		--zip-file fileb://lfs-snapshot/function.zip \
		--environment Variables="{LFS_BUCKET=$(LFS_BUCKET),SNAPSHOT_BUCKET=$(SNAPSHOT_BUCKET),SNAPSHOT_PREFIX=$(SNAPSHOT_PREFIX),EXTRA_SNAPSHOT_BUCKETS=$(EXTRA_SNAPSHOT_BUCKETS)}" \
		--timeout 300 \
		--region $(REGION) \
		--profile $(PROFILE)
	aws lambda create-function-url-config \
		--function-name $(SNAPSHOT_NAME) \
		--auth-type NONE \
		--region $(REGION) \
		--profile $(PROFILE)
	aws lambda add-permission \
		--function-name $(SNAPSHOT_NAME) \
		--statement-id FunctionURLAllowPublicAccess \
		--action lambda:InvokeFunctionUrl \
		--principal "*" \
		--function-url-auth-type NONE \
		--region $(REGION) \
		--profile $(PROFILE)
	aws lambda add-permission \
		--function-name $(SNAPSHOT_NAME) \
		--statement-id FunctionURLAllowInvoke \
		--action lambda:InvokeFunction \
		--principal "*" \
		--region $(REGION) \
		--profile $(PROFILE)

create: create-broker create-snapshot

# -----------------------------------------------------------------------
# Deploy (update existing functions)
# -----------------------------------------------------------------------

deploy-broker: build-broker
	aws lambda update-function-code \
		--function-name $(BROKER_NAME) \
		--zip-file fileb://lfs-broker/function.zip \
		--region $(REGION) \
		--profile $(PROFILE)

deploy-snapshot: build-snapshot
	aws lambda update-function-code \
		--function-name $(SNAPSHOT_NAME) \
		--zip-file fileb://lfs-snapshot/function.zip \
		--region $(REGION) \
		--profile $(PROFILE)

deploy: deploy-broker deploy-snapshot

# -----------------------------------------------------------------------
# Logs
# -----------------------------------------------------------------------

logs-broker:
	aws logs tail /aws/lambda/$(BROKER_NAME) \
		--region $(REGION) \
		--profile $(PROFILE) \
		--follow

logs-snapshot:
	aws logs tail /aws/lambda/$(SNAPSHOT_NAME) \
		--region $(REGION) \
		--profile $(PROFILE) \
		--follow

# -----------------------------------------------------------------------
# Info
# -----------------------------------------------------------------------

url-broker:
	aws lambda get-function-url-config \
		--function-name $(BROKER_NAME) \
		--region $(REGION) \
		--profile $(PROFILE) \
		--query FunctionUrl \
		--output text

url-snapshot:
	aws lambda get-function-url-config \
		--function-name $(SNAPSHOT_NAME) \
		--region $(REGION) \
		--profile $(PROFILE) \
		--query FunctionUrl \
		--output text

# -----------------------------------------------------------------------
# Secrets
# -----------------------------------------------------------------------

list-curators:
	aws secretsmanager get-secret-value \
		--secret-id github-lfs-api-keys \
		--region $(REGION) \
		--profile $(PROFILE) \
		--query SecretString \
		--output text | jq 'keys'

# -----------------------------------------------------------------------
# Snapshot
# -----------------------------------------------------------------------

full-snapshot:
	$(eval TOKEN := $(shell gh auth token --hostname $(GITHUB_HOST)))
	$(eval URL := $(shell make url-snapshot --no-print-directory))
	curl -X POST $(URL) \
		-H "Content-Type: application/json" \
		-d '{"repo":"$(REPO)","ref":"$(REF)","token":"$(TOKEN)","github_host":"$(GITHUB_HOST)"}'

.PHONY: init init-broker init-snapshot \
        configure-bucket \
        build build-broker build-snapshot \
        create create-broker create-snapshot \
        deploy deploy-broker deploy-snapshot \
        logs-broker logs-snapshot \
        url-broker url-snapshot \
        list-curators full-snapshot
