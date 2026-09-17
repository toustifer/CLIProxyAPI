// Command fetch_antigravity_models connects to the Antigravity API using the
// stored auth credentials and saves the dynamically fetched model list to a
// JSON file for inspection or offline use.
//
// Usage:
//
//	go run ./cmd/fetch_antigravity_models [flags]
//
// Flags:
//
//	--auths-dir <path>  Directory containing auth JSON files (default: config auth-dir)
//	--config    <path>  Config file path                 (default: "config.yaml")
//	--output    <path>  Output JSON file path             (default: "antigravity_models.json")
//	--pretty            Pretty-print the output JSON      (default: true)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	antigravityBaseURLDaily        = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandboxBaseURLDaily = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityBaseURLProd         = "https://cloudcode-pa.googleapis.com"
	antigravityModelsPath          = "/v1internal:fetchAvailableModels"
	maxFetchAttemptsPerEndpoint    = 2

	// antigravityQuotaRequestTimeout bounds a single fetchAvailableModels call.
	// Applied per request so one slow base URL cannot consume the budget of the
	// endpoints tried after it.
	antigravityQuotaRequestTimeout = 20 * time.Second
)

func init() {
	logging.SetupBaseLogger()
	log.SetLevel(log.InfoLevel)
}

// modelOutput wraps the fetched model list with fetch metadata.
type modelOutput struct {
	Models []modelEntry `json:"models"`
}

// modelEntry contains only the fields we want to keep for static model definitions.
type modelEntry struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	OwnedBy             string `json:"owned_by"`
	Type                string `json:"type"`
	DisplayName         string `json:"display_name"`
	Name                string `json:"name"`
	Description         string `json:"description"`
	ContextLength       int    `json:"context_length,omitempty"`
	MaxCompletionTokens int    `json:"max_completion_tokens,omitempty"`
}

func main() {
	var authsDir string
	var configPath string
	var outputPath string
	var pretty bool
	var quotaMode bool

	flag.StringVar(&authsDir, "auths-dir", "", "Directory containing auth JSON files (overrides config auth-dir)")
	flag.StringVar(&configPath, "config", "", "Configure File Path")
	flag.StringVar(&outputPath, "output", "antigravity_models.json", "Output JSON file path")
	flag.BoolVar(&pretty, "pretty", true, "Pretty-print the output JSON")
	flag.BoolVar(&quotaMode, "quota", false, "Report per-account quota for every enabled auth instead of writing the model list")
	flag.Parse()
	authsDirOverridden := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "auths-dir" {
			authsDirOverridden = true
		}
	})

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot get working directory: %v\n", err)
		os.Exit(1)
	}

	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(wd, "config.yaml")
	}
	cfg, err := config.LoadConfigOptional(configPath, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to load config file %s: %v\n", configPath, err)
		os.Exit(1)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}

	if !authsDirOverridden {
		authsDir = cfg.AuthDir
	} else if strings.TrimSpace(authsDir) != "" && !strings.HasPrefix(strings.TrimSpace(authsDir), "~") && !filepath.IsAbs(authsDir) {
		authsDir = filepath.Join(wd, authsDir)
	}
	if authsDir, err = util.ResolveAuthDir(authsDir); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to resolve auth directory: %v\n", err)
		os.Exit(1)
	}
	if _, errStat := os.Stat(authsDir); errStat != nil && !authsDirOverridden {
		localAuths := filepath.Join(wd, "auths")
		if fi, errLocal := os.Stat(localAuths); errLocal == nil && fi.IsDir() {
			authsDir = localAuths
		}
	}
	if realDir, errSym := filepath.EvalSymlinks(authsDir); errSym == nil {
		authsDir = realDir
	}
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(wd, outputPath)
	}

	fmt.Printf("Scanning auth files in: %s\n", authsDir)

	// Load all auth records from the directory.
	fileStore := sdkauth.NewFileTokenStore()
	fileStore.SetBaseDir(authsDir)

	ctx := context.Background()
	auths, err := fileStore.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to list auth files: %v\n", err)
		os.Exit(1)
	}
	if len(auths) == 0 {
		fmt.Fprintf(os.Stderr, "error: no auth files found in %s\n", authsDir)
		os.Exit(1)
	}

	// Find enabled antigravity auths.
	var agAuths []*coreauth.Auth
	for _, a := range auths {
		if a == nil || a.Disabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(a.Provider), "antigravity") {
			if !strings.Contains(a.ID, ".back") {
				agAuths = append(agAuths, a)
			}
		}
	}
	if len(agAuths) == 0 {
		for _, a := range auths {
			if a != nil && !a.Disabled && strings.EqualFold(strings.TrimSpace(a.Provider), "antigravity") {
				agAuths = append(agAuths, a)
			}
		}
	}
	if len(agAuths) == 0 {
		fmt.Fprintf(os.Stderr, "error: no enabled antigravity auth found in %s\n", authsDir)
		os.Exit(1)
	}

	// Quota report is a separate mode: it must inspect every auth, not just the
	// first one that answers, so it runs before the single-auth model fetch.
	if quotaMode {
		os.Exit(reportQuota(ctx, agAuths, cfg))
	}

	// Fetch models from the upstream Antigravity API using available auths.
	var models []modelEntry
	for _, chosen := range agAuths {
		fmt.Printf("Using auth: id=%s label=%s\n", chosen.ID, chosen.Label)
		fmt.Println("Fetching Antigravity model list from upstream...")

		fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		models = fetchModels(fetchCtx, chosen, cfg)
		cancel()

		if len(models) > 0 {
			fmt.Printf("Fetched %d models.\n", len(models))
			break
		}
		fmt.Fprintln(os.Stderr, "warning: no models returned from this auth, trying next...")
	}
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "warning: no models returned from any auth (API may be unavailable or tokens expired)")
	}

	// Build the output payload.
	out := modelOutput{
		Models: models,
	}

	// Marshal to JSON.
	var raw []byte
	if pretty {
		raw, err = json.MarshalIndent(out, "", "  ")
	} else {
		raw, err = json.Marshal(out)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to marshal JSON: %v\n", err)
		os.Exit(1)
	}

	if err = os.WriteFile(outputPath, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to write output file %s: %v\n", outputPath, err)
		os.Exit(1)
	}

	fmt.Printf("Model list saved to: %s\n", outputPath)
}

func defaultAntigravityFetchBaseURLs() []string {
	return []string{antigravityBaseURLDaily, antigravityBaseURLProd, antigravitySandboxBaseURLDaily}
}

func fetchModels(ctx context.Context, auth *coreauth.Auth, cfg *config.Config) []modelEntry {
	return fetchModelsFromBaseURLs(ctx, auth, defaultAntigravityFetchBaseURLs(), nil, cfg)
}

func fetchModelsFromBaseURLs(ctx context.Context, auth *coreauth.Auth, baseURLs []string, client *http.Client, cfg ...*config.Config) []modelEntry {
	var cfgObj *config.Config
	if len(cfg) > 0 {
		cfgObj = cfg[0]
	}
	var accessToken string
	if auth != nil {
		accessToken = metaStringValue(auth.Metadata, "access_token")
	}
	if accessToken == "" {
		fmt.Fprintln(os.Stderr, "error: no access token found in auth")
		return nil
	}

	for _, baseURL := range baseURLs {
		modelsURL := baseURL + antigravityModelsPath

		var payload []byte
		if auth != nil && auth.Metadata != nil {
			if pid, ok := auth.Metadata["project_id"].(string); ok && strings.TrimSpace(pid) != "" {
				payload = []byte(fmt.Sprintf(`{"project": "%s"}`, strings.TrimSpace(pid)))
			}
		}
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}

		for attempt := 1; attempt <= maxFetchAttemptsPerEndpoint; attempt++ {
			httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, modelsURL, strings.NewReader(string(payload)))
			if errReq != nil {
				continue
			}
			httpReq.Close = true
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+accessToken)
			ua := misc.AntigravityUserAgent()
			if auth != nil {
				if configuredUA := strings.TrimSpace(auth.Attributes["user_agent"]); configuredUA != "" {
					ua = misc.AntigravityRequestUserAgent(configuredUA)
				} else if metaUA, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(metaUA) != "" {
					ua = misc.AntigravityRequestUserAgent(metaUA)
				}
			}
			if ua == misc.AntigravityUserAgent() && cfgObj != nil && strings.TrimSpace(cfgObj.Antigravity.UserAgent) != "" {
				ua = misc.AntigravityRequestUserAgent(cfgObj.Antigravity.UserAgent)
			}
			httpReq.Header.Set("User-Agent", ua)

			httpClient := client
			if httpClient == nil {
				httpClient = &http.Client{Timeout: 30 * time.Second}
				if auth != nil {
					if transport, _, errProxy := proxyutil.BuildHTTPTransport(auth.ProxyURL); errProxy == nil && transport != nil {
						httpClient.Transport = transport
					}
				}
			}
			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				continue
			}

			bodyBytes, errRead := io.ReadAll(httpResp.Body)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
			if errRead != nil {
				continue
			}

			if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
				continue
			}

			result := gjson.GetBytes(bodyBytes, "models")
			if !result.Exists() {
				continue
			}

			var models []modelEntry

			for originalName, modelData := range result.Map() {
				modelID := strings.TrimSpace(originalName)
				if modelID == "" {
					continue
				}
				// Skip internal/experimental models
				switch modelID {
				case "chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview", "gemini-2.5-flash-thinking", "gemini-2.5-pro":
					continue
				}

				displayName := modelData.Get("displayName").String()
				if displayName == "" {
					displayName = modelID
				}

				entry := modelEntry{
					ID:          modelID,
					Object:      "model",
					OwnedBy:     "antigravity",
					Type:        "antigravity",
					DisplayName: displayName,
					Name:        modelID,
					Description: displayName,
				}

				if maxTok := modelData.Get("maxTokens").Int(); maxTok > 0 {
					entry.ContextLength = int(maxTok)
				}
				if maxOut := modelData.Get("maxOutputTokens").Int(); maxOut > 0 {
					entry.MaxCompletionTokens = int(maxOut)
				}

				models = append(models, entry)
			}

			return models
		}
	}

	return nil
}

func metaStringValue(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	default:
		return ""
	}
}

// resolveToolUserAgent applies the same precedence as the runtime executor:
// auth attributes, then auth metadata, then config, then the dynamic default.
// Keeping the two in sync matters because Google gates Antigravity by the
// client version advertised in this header.
func resolveToolUserAgent(auth *coreauth.Auth, cfg *config.Config) string {
	var raw string
	if auth != nil {
		if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
			raw = ua
		} else if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
			raw = strings.TrimSpace(ua)
		}
	}
	if raw == "" && cfg != nil {
		raw = strings.TrimSpace(cfg.Antigravity.UserAgent)
	}
	if raw == "" {
		return misc.AntigravityUserAgent()
	}
	return misc.AntigravityRequestUserAgent(raw)
}

// fetchAvailableModelsRaw returns the raw fetchAvailableModels payload for one
// auth. It deliberately uses the access token already stored in the auth file
// and never refreshes it: refreshing out of band invalidates the token the
// running server holds, which shows up there as a 401 until it refreshes again.
func fetchAvailableModelsRaw(ctx context.Context, auth *coreauth.Auth, cfg *config.Config) ([]byte, error) {
	if auth == nil {
		return nil, errors.New("nil auth")
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	if accessToken == "" {
		return nil, errors.New("no access_token in auth metadata")
	}

	payload := []byte(`{}`)
	if pid, ok := auth.Metadata["project_id"].(string); ok && strings.TrimSpace(pid) != "" {
		payload = []byte(fmt.Sprintf(`{"project": %q}`, strings.TrimSpace(pid)))
	}

	// Per-auth proxy wins, then the global config proxy, then direct. Without
	// the config fallback this tool cannot reach Google from networks that
	// require a proxy even though the server itself works fine.
	proxyRaw := strings.TrimSpace(auth.ProxyURL)
	if proxyRaw == "" && cfg != nil {
		proxyRaw = strings.TrimSpace(cfg.ProxyURL)
	}

	var lastErr error
	for _, baseURL := range defaultAntigravityFetchBaseURLs() {
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+antigravityModelsPath, strings.NewReader(string(payload)))
		if errReq != nil {
			lastErr = errReq
			continue
		}
		httpReq.Close = true
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+accessToken)
		httpReq.Header.Set("User-Agent", resolveToolUserAgent(auth, cfg))

		// The timeout is per request, not shared across endpoints: one deadline
		// for the whole loop starves the later base URLs and reports the wrong
		// endpoint as the failing one.
		httpClient := &http.Client{Timeout: antigravityQuotaRequestTimeout}
		if transport, _, errProxy := proxyutil.BuildHTTPTransport(proxyRaw); errProxy == nil && transport != nil {
			httpClient.Transport = transport
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			lastErr = errDo
			continue
		}
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		if errRead != nil {
			lastErr = errRead
			continue
		}
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			lastErr = fmt.Errorf("upstream status %d: %s", httpResp.StatusCode, truncateForLog(string(bodyBytes), 200))
			continue
		}
		return bodyBytes, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no endpoint returned a response")
	}
	return nil, lastErr
}

func truncateForLog(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// quotaBucket groups models that share one upstream quota allowance. Antigravity
// meters quota per model family, so several models often report the identical
// remaining fraction and reset instant.
type quotaBucket struct {
	Remaining float64
	HasRemain bool
	ResetRaw  string
	Models    []string
}

// reportQuota prints a per-account quota report and returns a process exit code.
func reportQuota(ctx context.Context, auths []*coreauth.Auth, cfg *config.Config) int {
	fmt.Printf("Quota report / 额度报告  (%d auths)\n", len(auths))
	fmt.Println(strings.Repeat("=", 78))

	failures := 0
	for _, auth := range auths {
		label := strings.TrimSpace(auth.Label)
		if label == "" {
			label = auth.ID
		}
		fmt.Printf("\n--- %s ---\n", label)

		// No per-account deadline here: each request inside the helper carries
		// its own timeout, so a slow endpoint cannot starve the ones after it.
		raw, errFetch := fetchAvailableModelsRaw(ctx, auth, cfg)
		if errFetch != nil {
			failures++
			fmt.Printf("  FAILED / 获取失败: %s\n", truncateForLog(errFetch.Error(), 220))
			continue
		}

		buckets, errParse := parseQuotaBuckets(raw)
		if errParse != nil {
			failures++
			fmt.Printf("  FAILED / 解析失败: %v\n", errParse)
			continue
		}
		if len(buckets) == 0 {
			fmt.Println("  no quota info returned / 上游未返回额度信息")
			continue
		}

		total := 0
		for _, b := range buckets {
			total += len(b.Models)
		}
		fmt.Printf("  models=%d buckets=%d\n", total, len(buckets))
		for _, b := range buckets {
			remaining := "  0.0% (exhausted)"
			if b.HasRemain {
				remaining = fmt.Sprintf("%5.1f%%", b.Remaining*100)
			}
			reset := b.ResetRaw
			if reset == "" {
				reset = "-"
			} else {
				reset = formatReset(reset)
			}
			fmt.Printf("  [remaining / 剩余 %s]  reset / 重置 %s\n", remaining, reset)
			fmt.Printf("      %s\n", strings.Join(b.Models, ", "))
		}
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("Done / 完成. accounts=%d failed=%d\n", len(auths), failures)
	if failures == len(auths) {
		return 1
	}
	return 0
}

// parseQuotaBuckets groups models by their reported quota allowance.
func parseQuotaBuckets(raw []byte) ([]quotaBucket, error) {
	result := gjson.GetBytes(raw, "models")
	if !result.Exists() {
		return nil, errors.New(`response has no "models" object`)
	}

	byKey := map[string]*quotaBucket{}
	order := []string{}
	for name, modelData := range result.Map() {
		modelID := strings.TrimSpace(name)
		if modelID == "" {
			continue
		}
		quota := modelData.Get("quotaInfo")
		bucket := &quotaBucket{ResetRaw: quota.Get("resetTime").String()}
		if frac := quota.Get("remainingFraction"); frac.Exists() {
			bucket.Remaining = frac.Float()
			bucket.HasRemain = true
		}
		key := fmt.Sprintf("%t|%.6f|%s", bucket.HasRemain, bucket.Remaining, bucket.ResetRaw)
		existing, ok := byKey[key]
		if !ok {
			byKey[key] = bucket
			existing = bucket
			order = append(order, key)
		}
		existing.Models = append(existing.Models, modelID)
	}

	buckets := make([]quotaBucket, 0, len(order))
	for _, key := range order {
		b := byKey[key]
		sort.Strings(b.Models)
		buckets = append(buckets, *b)
	}
	// Exhausted buckets first, then most-remaining first: the actionable end.
	sort.SliceStable(buckets, func(i, j int) bool {
		if buckets[i].HasRemain != buckets[j].HasRemain {
			return !buckets[i].HasRemain
		}
		return buckets[i].Remaining < buckets[j].Remaining
	})
	return buckets, nil
}

// formatReset renders an RFC3339 reset instant as UTC plus local time.
func formatReset(raw string) string {
	parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if errParse != nil {
		return raw
	}
	return fmt.Sprintf("%s (local %s)", parsed.UTC().Format("2006-01-02T15:04:05Z"), parsed.Local().Format("2006-01-02 15:04"))
}
