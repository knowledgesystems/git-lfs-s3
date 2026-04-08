package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// SnapshotRequest is the payload sent by the GitHub Action or curl
type SnapshotRequest struct {
	Repo         string   `json:"repo"`          // e.g. "cBioPortal/datahub"
	Ref          string   `json:"ref"`           // e.g. "master"
	Token        string   `json:"token"`         // GitHub token for API auth
	GitHubHost   string   `json:"github_host"`   // e.g. "github.com" or "github.mskcc.org"
	ChangedFiles []string `json:"changed_files"` // optional: incremental mode
}

// SnapshotResult summarizes what was done
type SnapshotResult struct {
	Copied  int      `json:"copied"`
	Skipped int      `json:"skipped"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
}

// GitHubTreeResponse is the response from the GitHub tree API
type GitHubTreeResponse struct {
	Tree []GitHubTreeEntry `json:"tree"`
}

type GitHubTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // "blob" or "tree"
	SHA  string `json:"sha"`
}

// GitHubContentResponse is the response from the GitHub contents API
type GitHubContentResponse struct {
	Content  string `json:"content"`  // base64 encoded
	Encoding string `json:"encoding"` // "base64"
	Size     int    `json:"size"`
}

// LFSPointer represents a parsed Git LFS pointer file
type LFSPointer struct {
	OID  string
	Size int64
}

var (
	s3Client       *s3.Client
	lfsBucket      string
	snapshotBucket string
	snapshotPrefix string
	extraBuckets   []string
	httpClient     = &http.Client{}
)

func init() {
	lfsBucket = os.Getenv("LFS_BUCKET")
	if lfsBucket == "" {
		log.Fatal("LFS_BUCKET environment variable is required")
	}

	snapshotBucket = os.Getenv("SNAPSHOT_BUCKET")
	if snapshotBucket == "" {
		log.Fatal("SNAPSHOT_BUCKET environment variable is required")
	}

	// SNAPSHOT_PREFIX is optional. If set, snapshot files are written under
	// this prefix in the snapshot bucket. If empty or unset, files are written
	// at the bucket root. Example: "public/" writes to public/brca_tcga/...
	snapshotPrefix = os.Getenv("SNAPSHOT_PREFIX")

	// EXTRA_SNAPSHOT_BUCKETS is optional. If set, snapshot files are also
	// written to these additional buckets. Comma-separated list of bucket names.
	// Each bucket receives the same files at the same paths as the primary
	// snapshot bucket. If empty or unset, only the primary bucket is written to.
	if raw := os.Getenv("EXTRA_SNAPSHOT_BUCKETS"); raw != "" {
		extraBuckets = strings.Split(raw, ",")
	}

	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	s3Client = s3.NewFromConfig(cfg)

	log.Printf("lfs bucket: %s, snapshot bucket: %s, snapshot prefix: %q, extra buckets: %v",
		lfsBucket, snapshotBucket, snapshotPrefix, extraBuckets)
}

// getAPIBaseURL returns the GitHub API base URL for the given host.
// Defaults to api.github.com for github.com or empty host.
// For GitHub Enterprise, uses the /api/v3 prefix.
func getAPIBaseURL(host string) string {
	if host == "" || host == "github.com" {
		return "https://api.github.com"
	}
	return fmt.Sprintf("https://%s/api/v3", host)
}

// handler receives requests from the Lambda Function URL, decodes the body,
// and delegates to runSnapshot
func handler(ctx context.Context, urlReq events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	// Handle base64 encoded body (Lambda Function URLs encode the body)
	body := urlReq.Body
	if urlReq.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(urlReq.Body)
		if err != nil {
			log.Printf("failed to decode base64 body: %v", err)
			return errorResponse(500, "failed to decode request body"), nil
		}
		body = string(decoded)
	}

	// Parse the snapshot request
	var req SnapshotRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		log.Printf("failed to parse request body: %v", err)
		return errorResponse(400, "invalid request body"), nil
	}

	result, err := runSnapshot(ctx, req)
	if err != nil {
		log.Printf("snapshot failed: %v", err)
		return errorResponse(500, err.Error()), nil
	}

	respBody, _ := json.Marshal(result)
	return events.LambdaFunctionURLResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(respBody),
	}, nil
}

// runSnapshot contains the core snapshot logic
func runSnapshot(ctx context.Context, req SnapshotRequest) (SnapshotResult, error) {
	if req.Repo == "" {
		return SnapshotResult{}, fmt.Errorf("repo is required")
	}
	if req.Ref == "" {
		req.Ref = "master"
	}
	if req.GitHubHost == "" {
		req.GitHubHost = "github.com"
	}

	apiBase := getAPIBaseURL(req.GitHubHost)
	log.Printf("starting snapshot: repo=%s ref=%s host=%s api=%s prefix=%q extra_buckets=%v",
		req.Repo, req.Ref, req.GitHubHost, apiBase, snapshotPrefix, extraBuckets)

	var paths []string
	var err error

	if len(req.ChangedFiles) > 0 {
		// Incremental mode - only process changed files
		log.Printf("incremental mode: %d changed files", len(req.ChangedFiles))
		paths = req.ChangedFiles
	} else {
		// Full mode - walk the entire tree
		log.Printf("full snapshot mode")
		paths, err = getRepoTree(ctx, apiBase, req.Repo, req.Ref, req.Token)
		if err != nil {
			return SnapshotResult{}, fmt.Errorf("failed to get repo tree: %w", err)
		}
		log.Printf("found %d files in repo tree", len(paths))
	}

	result := SnapshotResult{}

	for _, path := range paths {
		err := processFile(ctx, apiBase, req.Repo, req.Ref, req.Token, path)
		if err != nil {
			log.Printf("error processing %s: %v", path, err)
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		result.Copied++
	}

	log.Printf("snapshot complete: copied=%d failed=%d", result.Copied, result.Failed)
	return result, nil
}

// getRepoTree fetches the full file tree from the GitHub API
func getRepoTree(ctx context.Context, apiBase, repo, ref, token string) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", apiBase, repo, ref)

	body, err := githubGet(ctx, url, token)
	if err != nil {
		return nil, err
	}

	var treeResp GitHubTreeResponse
	if err := json.Unmarshal(body, &treeResp); err != nil {
		return nil, fmt.Errorf("failed to parse tree response: %w", err)
	}

	var paths []string
	for _, entry := range treeResp.Tree {
		if entry.Type == "blob" {
			paths = append(paths, entry.Path)
		}
	}

	return paths, nil
}

// processFile fetches a file from GitHub, determines if it is an LFS pointer
// or a regular file, then copies the actual content to the snapshot bucket
// and any extra buckets under the configured SNAPSHOT_PREFIX.
func processFile(ctx context.Context, apiBase, repo, ref, token, path string) error {
	url := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", apiBase, repo, path, ref)

	body, err := githubGet(ctx, url, token)
	if err != nil {
		return fmt.Errorf("failed to fetch file metadata: %w", err)
	}

	var contentResp GitHubContentResponse
	if err := json.Unmarshal(body, &contentResp); err != nil {
		return fmt.Errorf("failed to parse content response: %w", err)
	}

	// Decode the base64 content from GitHub
	// GitHub adds newlines to the base64 content so we strip them first
	cleaned := strings.ReplaceAll(contentResp.Content, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return fmt.Errorf("failed to decode file content: %w", err)
	}

	content := string(decoded)

	// Prepend the snapshot prefix to the destination path.
	// If SNAPSHOT_PREFIX is empty, files are written at the bucket root.
	snapshotPath := snapshotPrefix + path

	// Build the full list of destination buckets — primary + any extras
	allBuckets := append([]string{snapshotBucket}, extraBuckets...)

	// Check if this is an LFS pointer
	pointer, isLFS := parseLFSPointer(content)
	if isLFS {
		log.Printf("LFS file: %s -> %s (oid: %s)", path, snapshotPath, pointer.OID)
		for _, bucket := range allBuckets {
			if err := streamLFSObjectToSnapshot(ctx, pointer, bucket, snapshotPath); err != nil {
				return fmt.Errorf("failed to write to bucket %s: %w", bucket, err)
			}
		}
		return nil
	}

	// Regular file — write content directly to all snapshot buckets
	log.Printf("regular file: %s -> %s (%d bytes)", path, snapshotPath, len(decoded))
	for _, bucket := range allBuckets {
		if err := writeToSnapshot(ctx, bucket, snapshotPath, decoded); err != nil {
			return fmt.Errorf("failed to write to bucket %s: %w", bucket, err)
		}
	}
	return nil
}

// parseLFSPointer checks if content is a Git LFS pointer and parses it
func parseLFSPointer(content string) (*LFSPointer, bool) {
	if !strings.HasPrefix(content, "version https://git-lfs.github.com/spec/v1") {
		return nil, false
	}

	pointer := &LFSPointer{}
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "oid sha256:") {
			pointer.OID = strings.TrimSpace(strings.TrimPrefix(line, "oid sha256:"))
		}
		if strings.HasPrefix(line, "size ") {
			fmt.Sscanf(strings.TrimPrefix(line, "size "), "%d", &pointer.Size)
		}
	}

	if pointer.OID == "" {
		return nil, false
	}

	return pointer, true
}

// streamLFSObjectToSnapshot streams an LFS object from the LFS bucket to the
// destination bucket using GetObject + PutObject. Data flows through the Lambda
// as a stream rather than being fully buffered in memory, keeping memory usage
// constant regardless of file size. Safe for genomics files of any size.
func streamLFSObjectToSnapshot(ctx context.Context, pointer *LFSPointer, destBucket, destPath string) error {
	if len(pointer.OID) < 4 {
		return fmt.Errorf("invalid OID: %s", pointer.OID)
	}

	// LFS content-addressable key
	srcKey := fmt.Sprintf("lfs/objects/%s/%s/%s",
		pointer.OID[:2], pointer.OID[2:4], pointer.OID)

	// Get the object from the LFS bucket as a stream
	getResult, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(lfsBucket),
		Key:    aws.String(srcKey),
	})
	if err != nil {
		return fmt.Errorf("failed to get LFS object %s: %w", pointer.OID, err)
	}
	defer getResult.Body.Close()

	// Stream directly into the destination bucket at the destination path.
	// The Body io.Reader is never fully buffered — data flows through the
	// Lambda in chunks, keeping memory usage constant regardless of file size.
	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(destBucket),
		Key:           aws.String(destPath),
		Body:          getResult.Body,
		ContentLength: getResult.ContentLength,
	})
	if err != nil {
		return fmt.Errorf("failed to stream LFS object %s to %s: %w", pointer.OID, destBucket, err)
	}

	log.Printf("streamed LFS object %s -> %s/%s", pointer.OID, destBucket, destPath)
	return nil
}

// writeToSnapshot writes regular (non-LFS) file content to the destination bucket
func writeToSnapshot(ctx context.Context, bucket, path string, content []byte) error {
	_, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
		Body:   strings.NewReader(string(content)),
	})
	if err != nil {
		return fmt.Errorf("failed to write %s to %s: %w", path, bucket, err)
	}

	log.Printf("wrote regular file -> %s/%s", bucket, path)
	return nil
}

// githubGet makes an authenticated GET request to the GitHub API
func githubGet(ctx context.Context, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// errorResponse returns a Lambda Function URL response with an error message
func errorResponse(statusCode int, message string) events.LambdaFunctionURLResponse {
	body, _ := json.Marshal(map[string]string{"error": message})
	return events.LambdaFunctionURLResponse{
		StatusCode: statusCode,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}
}

func main() {
	lambda.Start(handler)
}
