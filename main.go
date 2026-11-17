// fiorano-to-wso2-apim-migrator is a small CLI for migrating a Fiorano REST API export
// into WSO2 API Manager, as a set of subcommands sharing two plain
// properties files instead of one monolithic script:
//
//	config.properties    host, admin credentials, environment/vhost
//	backends.properties  per-API id/name/context/backend-url, plus the
//	                      created API Product's id/name/context
//
// Subcommands:
//
//	split          <project_dir> [--out dir] [--version X.Y]
//	create-apis    --config FILE --backends FILE --split-dir DIR --project-name NAME [--version X.Y] [--mock-base-url URL]
//	create-product --config FILE --backends FILE --split-dir DIR --project-name NAME [--product-name NAME] [--product-context PATH] [--version X.Y]
//	update-backends --config FILE --backends FILE
//
// Never modifies the original Fiorano export. Credentials live only in
// config.properties (keep it out of anything you share) — backends.properties
// holds no secrets, just ids/urls, so it's safe to send back and forth while
// backend addresses trickle in.
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ===========================================================================
// properties files: simple ordered key=value, # comments preserved on reload
// ===========================================================================

// properties keeps the file's original lines verbatim — comments, blank
// lines, section headers — so rewriting it (e.g. to save a newly-registered
// clientId) only changes the one line that actually changed, or appends a
// new line for a brand-new key. Nothing you wrote in the file is ever
// silently dropped on save.
type properties struct {
	order    []string // key order, for Keys()
	data     map[string]string
	rawLines []string // the file's lines verbatim, kept in sync by Set()
	lineOf   map[string]int
}

func newProperties() *properties {
	return &properties{data: map[string]string{}, lineOf: map[string]int{}}
}

func loadProperties(path string) (*properties, error) {
	p := newProperties()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := sc.Text()
		p.rawLines = append(p.rawLines, raw)
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		p.order = append(p.order, key)
		p.data[key] = val
		p.lineOf[key] = len(p.rawLines) - 1
	}
	return p, sc.Err()
}

func (p *properties) Get(key, def string) string {
	if v, ok := p.data[key]; ok && v != "" {
		return v
	}
	return def
}

func (p *properties) Set(key, val string) {
	line := key + "=" + val
	if idx, ok := p.lineOf[key]; ok {
		p.rawLines[idx] = line
	} else {
		p.order = append(p.order, key)
		p.rawLines = append(p.rawLines, line)
		p.lineOf[key] = len(p.rawLines) - 1
	}
	p.data[key] = val
}

func (p *properties) Keys() []string {
	return append([]string{}, p.order...)
}

func (p *properties) Save(path string) error {
	var b strings.Builder
	for _, l := range p.rawLines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// backendEntry is one api.<key>.* group read out of backends.properties.
type backendEntry struct {
	Key     string
	Name    string
	Context string
	ID      string
	Backend string
}

func backendKeys(p *properties) []string {
	seen := map[string]bool{}
	var keys []string
	re := regexp.MustCompile(`^api\.([^.]+)\.`)
	for _, k := range p.Keys() {
		if m := re.FindStringSubmatch(k); m != nil {
			if !seen[m[1]] {
				seen[m[1]] = true
				keys = append(keys, m[1])
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func getBackendEntry(p *properties, key string) backendEntry {
	return backendEntry{
		Key:     key,
		Name:    p.Get("api."+key+".name", ""),
		Context: p.Get("api."+key+".context", ""),
		ID:      p.Get("api."+key+".id", ""),
		Backend: p.Get("api."+key+".backend", ""),
	}
}

func setBackendEntry(p *properties, e backendEntry) {
	p.Set("api."+e.Key+".name", e.Name)
	p.Set("api."+e.Key+".context", e.Context)
	p.Set("api."+e.Key+".id", e.ID)
	p.Set("api."+e.Key+".backend", e.Backend)
}

// ===========================================================================
// XML shapes matching Fiorano's resources/<Name>.xml ResourceConfiguration
// ===========================================================================

type policyEntry struct {
	PolicyID string `xml:"PolicyID"`
}
type policies struct {
	Entries []policyEntry `xml:"Entry"`
}
type endpointEntry struct {
	Key string `xml:"key"`
}
type endpoints struct {
	Entry endpointEntry `xml:"entry"`
}
type resourceConfiguration struct {
	ResourceID string    `xml:"resourceID"`
	BasePath   string    `xml:"basePath"`
	Endpoints  endpoints `xml:"endpoints"`
	Policies   policies  `xml:"Policies"`
}
type resource struct {
	ResourceID  string
	BasePath    string
	EndpointKey string
	PolicyIDs   []string
}

// ===========================================================================
// swagger text corrections (the "split" subcommand's own logic, unchanged
// and re-verified against the Python reference before this rewrite)
// ===========================================================================

var (
	reServersBlock   = regexp.MustCompile(`(?m)^servers:\n(?:-.*\n)+`)
	reEmptyContact   = regexp.MustCompile(`(?m)^\s*contact:\s*\{\}\s*\n`)
	reEmptyLicense   = regexp.MustCompile(`(?m)^\s*license:\s*\{\}\s*\n`)
	rePathLine       = regexp.MustCompile(`(?m)^(  )([A-Za-z0-9_\-/]+):$`)
	reOperationID    = regexp.MustCompile(`(?m)^\s*operationId:.*\n`)
	reEmptyExtension = regexp.MustCompile(`(?m)^\s*extensions:\s*\{\}\s*\n`)
	reResponseCode   = regexp.MustCompile(`(?m)^(\s+)(\d{3}):$`)
	reTitle          = regexp.MustCompile(`(title:\s*)"[^"]*"`)
	reVersion        = regexp.MustCompile(`(version:\s*)"[^"]*"`)
	rePathHeader     = regexp.MustCompile(`(?m)^  (/\S*):$`)
	reMethodLine     = regexp.MustCompile(`(?m)^    (get|post|put|delete|patch):$`)
)

func fixOpenAPIText(text string) string {
	text = reServersBlock.ReplaceAllString(text, "")
	text = reEmptyContact.ReplaceAllString(text, "")
	text = reEmptyLicense.ReplaceAllString(text, "")
	text = rePathLine.ReplaceAllStringFunc(text, func(m string) string {
		sub := rePathLine.FindStringSubmatch(m)
		indent, name := sub[1], sub[2]
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		return indent + name + ":"
	})
	text = reOperationID.ReplaceAllString(text, "")
	text = reEmptyExtension.ReplaceAllString(text, "")
	text = reResponseCode.ReplaceAllString(text, `$1"$2":`)
	return text
}

func splitOpenAPISections(fixedText string) (preamble string, blocks map[string]string, footer string, err error) {
	lines := splitKeepEnds(fixedText)
	pathsStart := -1
	for i, l := range lines {
		if strings.TrimRight(l, "\r\n") == "paths:" {
			pathsStart = i
			break
		}
	}
	if pathsStart == -1 {
		return "", nil, "", fmt.Errorf("no \"paths:\" line found in openapi.yaml")
	}
	footerStart := len(lines)
	reTopLevel := regexp.MustCompile(`^\S`)
	for i := pathsStart + 1; i < len(lines); i++ {
		if reTopLevel.MatchString(lines[i]) {
			footerStart = i
			break
		}
	}
	preamble = strings.Join(lines[:pathsStart+1], "")
	footer = strings.Join(lines[footerStart:], "")
	pathLines := lines[pathsStart+1 : footerStart]

	blocks = map[string]string{}
	currentPath := ""
	var currentLines []string
	flush := func() {
		if currentPath != "" {
			blocks[currentPath] = strings.Join(currentLines, "")
		}
	}
	for _, line := range pathLines {
		if m := rePathHeader.FindStringSubmatch(line); m != nil {
			flush()
			currentPath = m[1]
			currentLines = []string{line}
		} else {
			currentLines = append(currentLines, line)
		}
	}
	flush()
	return preamble, blocks, footer, nil
}

// pathMethods extracts {target, verb} pairs from a split API's swagger text,
// used by create-product to build its "operations" list without hardcoding
// any project's specific resource names.
func pathMethods(blocks map[string]string, paths []string) [][2]string {
	var out [][2]string
	for _, p := range paths {
		block, ok := blocks[p]
		if !ok {
			continue
		}
		for _, m := range reMethodLine.FindAllStringSubmatch(block, -1) {
			out = append(out, [2]string{p, strings.ToUpper(m[1])})
		}
	}
	return out
}

func splitKeepEnds(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func setTitle(preamble, newTitle, newVersion string) string {
	preamble = reTitle.ReplaceAllString(preamble, `${1}"`+newTitle+`"`)
	if newVersion != "" {
		replaced := false
		preamble = reVersion.ReplaceAllStringFunc(preamble, func(m string) string {
			if replaced {
				return m
			}
			replaced = true
			sub := reVersion.FindStringSubmatch(m)
			return sub[1] + `"` + newVersion + `"`
		})
	}
	return preamble
}

func parseResources(projectDir string) ([]resource, error) {
	resourcesDir := filepath.Join(projectDir, "resources")
	entries, err := os.ReadDir(resourcesDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", resourcesDir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".xml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []resource
	for _, name := range names {
		path := filepath.Join(resourcesDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		var rc resourceConfiguration
		if err := xml.Unmarshal(data, &rc); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		resourceID := rc.ResourceID
		if resourceID == "" {
			resourceID = strings.TrimSuffix(name, ".xml")
		}
		basePath := rc.BasePath
		if !strings.HasPrefix(basePath, "/") {
			basePath = "/" + basePath
		}
		var policyIDs []string
		for _, e := range rc.Policies.Entries {
			if e.PolicyID != "" {
				policyIDs = append(policyIDs, e.PolicyID)
			}
		}
		out = append(out, resource{
			ResourceID:  resourceID,
			BasePath:    basePath,
			EndpointKey: rc.Endpoints.Entry.Key,
			PolicyIDs:   policyIDs,
		})
	}
	return out, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ===========================================================================
// cmd: split
// ===========================================================================

// reorderArgs moves every "--flag value" pair before any bare positional
// token, working around the stdlib flag package's rule of stopping parsing
// at the first non-flag argument — so `split <dir> --out x` works the same
// as `split --out x <dir>`. Every flag here takes a value (none are bool
// switches), so each "--x" always consumes exactly one following token.
// confDir finds the "conf" folder next to wherever this binary is actually
// running from — <tool root>/bin/<this binary>  ->  <tool root>/conf — so
// config.properties/backends.properties are found correctly no matter what
// directory you were in when you ran the tool. Falls back to the current
// directory if the binary's own path can't be resolved (e.g. run via `go run`).
func confDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "."
	}
	binDir := filepath.Dir(exe)
	return filepath.Join(filepath.Dir(binDir), "conf")
}

// resolveConfPath turns a bare filename (e.g. "backends.properties") into a
// full path inside conf/; an already-absolute or explicitly-relative path
// (containing a "/") is left untouched.
func resolveConfPath(name string) string {
	if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return name
	}
	return filepath.Join(confDir(), name)
}

// logsDir is logs/, a sibling of bin/ and conf/ next to the running binary —
// every subcommand's activity for a project is appended here.
func logsDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "logs"
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "logs"
	}
	binDir := filepath.Dir(exe)
	return filepath.Join(filepath.Dir(binDir), "logs")
}

// openProjectLog opens (creating logs/ and the file if needed) logs/<projectName>.log
// in append mode, writes a run header, and returns writers that tee every
// subsequent write to both the terminal and that file. One log file per
// project accumulates the history of every subcommand run against it.
func openProjectLog(projectName, subcommand string) (outW, errW io.Writer, closeFn func() error, logPath string, err error) {
	dir := logsDir()
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return nil, nil, nil, "", fmt.Errorf("creating logs dir %s: %w", dir, mkErr)
	}
	logPath = filepath.Join(dir, projectName+".log")
	f, openErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if openErr != nil {
		return nil, nil, nil, "", fmt.Errorf("opening log file %s: %w", logPath, openErr)
	}
	outW = io.MultiWriter(os.Stdout, f)
	errW = io.MultiWriter(os.Stderr, f)
	fmt.Fprintf(outW, "\n=== %s %s run: %s ===\n", projectName, subcommand, time.Now().Format("2006-01-02 15:04:05 MST"))
	return outW, errW, f.Close, logPath, nil
}

func reorderArgs(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			if strings.Contains(a, "=") {
				flags = append(flags, a)
			} else if i+1 < len(args) {
				flags = append(flags, a, args[i+1])
				i++
			} else {
				flags = append(flags, a)
			}
		} else {
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}

func cmdSplit(args []string) error {
	fs := flag.NewFlagSet("split", flag.ExitOnError)
	configFlag := fs.String("config", resolveConfPath("config.properties"), "path to config.properties (default: conf/config.properties next to this binary)")
	outFlag := fs.String("out", "", "output directory (default: <project_dir>/wso2-migration, or outDir= in config.properties)")
	versionFlag := fs.String("version", "", "override API version in generated files (or apiVersion= in config.properties)")
	fs.Parse(reorderArgs(args))
	rest := fs.Args()

	cfg, _ := loadProperties(*configFlag) // split doesn't need host/admin, so a missing file is fine here

	projectDirArg := ""
	if len(rest) == 1 {
		projectDirArg = rest[0]
	} else if len(rest) == 0 {
		projectDirArg = cfg.Get("projectDir", "")
	}
	if projectDirArg == "" {
		return fmt.Errorf("usage: split <fiorano_project_dir> [--out dir] [--version X.Y]\n(or set projectDir= in %s and omit the argument)", *configFlag)
	}
	if *outFlag == "" {
		*outFlag = cfg.Get("outDir", "")
	}
	if *versionFlag == "" {
		*versionFlag = cfg.Get("apiVersion", "")
	}

	projectDir, err := filepath.Abs(projectDirArg)
	if err != nil {
		return err
	}
	info, err := os.Stat(projectDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("project directory not found: %s", projectDir)
	}

	projectName := filepath.Base(projectDir)
	if idx := strings.Index(projectName, "__"); idx != -1 {
		projectName = projectName[:idx]
	}

	outDir := *outFlag
	if outDir == "" {
		outDir = filepath.Join(projectDir, "wso2-migration")
	}
	outDir, err = filepath.Abs(outDir)
	if err != nil {
		return err
	}
	splitDir := filepath.Join(outDir, "split-by-endpoint")
	policiesOutDir := filepath.Join(outDir, "Fiorano_policies")
	if err := os.MkdirAll(splitDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(policiesOutDir, 0o755); err != nil {
		return err
	}

	outW, errW, closeLog, logPath, err := openProjectLog(projectName, "split")
	if err != nil {
		return err
	}
	defer closeLog()

	openAPIPath := filepath.Join(projectDir, "openapi.yaml")
	rawBytes, err := os.ReadFile(openAPIPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", openAPIPath, err)
	}
	fixedText := fixOpenAPIText(string(rawBytes))
	correctedPath := filepath.Join(outDir, "openapi_wso2.yaml")
	if err := os.WriteFile(correctedPath, []byte(fixedText), 0o644); err != nil {
		return err
	}
	preamble, pathBlocks, footer, err := splitOpenAPISections(fixedText)
	if err != nil {
		return err
	}

	resources, err := parseResources(projectDir)
	if err != nil {
		return err
	}

	groups := map[string][]resource{}
	var groupOrder []string
	var unrouted []resource
	for _, r := range resources {
		if r.EndpointKey != "" {
			if _, seen := groups[r.EndpointKey]; !seen {
				groupOrder = append(groupOrder, r.EndpointKey)
			}
			groups[r.EndpointKey] = append(groups[r.EndpointKey], r)
		} else {
			unrouted = append(unrouted, r)
		}
	}
	sortedKeys := append([]string{}, groupOrder...)
	sort.SliceStable(sortedKeys, func(i, j int) bool {
		return len(groups[sortedKeys[i]]) > len(groups[sortedKeys[j]])
	})

	targetsDir := filepath.Join(projectDir, "targets")
	type summaryRow struct {
		apiName     string
		endpointKey string
		count       int
		present     bool
	}
	var summaryRows []summaryRow
	policiesCopied := map[string]bool{}
	policiesMissing := map[string]bool{}

	for _, endpointKey := range sortedKeys {
		groupResources := groups[endpointKey]
		apiName := projectName + "-" + endpointKey
		var orderedPaths []string
		groupPaths := map[string]string{}
		for _, r := range groupResources {
			if block, ok := pathBlocks[r.BasePath]; ok {
				if _, already := groupPaths[r.BasePath]; !already {
					orderedPaths = append(orderedPaths, r.BasePath)
				}
				groupPaths[r.BasePath] = block
			} else {
				fmt.Fprintf(errW, "  WARNING: %s (%s) not found in openapi.yaml paths — skipped from %s\n", r.ResourceID, r.BasePath, apiName)
			}
		}
		apiPreamble := setTitle(preamble, apiName, *versionFlag)
		var body strings.Builder
		body.WriteString(apiPreamble)
		for _, p := range orderedPaths {
			body.WriteString(groupPaths[p])
		}
		body.WriteString(footer)
		outFile := filepath.Join(splitDir, apiName+".yaml")
		if err := os.WriteFile(outFile, []byte(body.String()), 0o644); err != nil {
			return err
		}
		for _, r := range groupResources {
			for _, pid := range r.PolicyIDs {
				if policiesCopied[pid] {
					continue
				}
				src := filepath.Join(projectDir, "policies", pid, "Production.xml")
				if fileExists(src) {
					destDir := filepath.Join(policiesOutDir, pid)
					if err := os.MkdirAll(destDir, 0o755); err != nil {
						return err
					}
					if err := copyFile(src, filepath.Join(destDir, "Production.xml")); err != nil {
						return err
					}
					policiesCopied[pid] = true
				} else {
					policiesMissing[pid] = true
				}
			}
		}
		targetPresent := dirExists(filepath.Join(targetsDir, endpointKey))
		summaryRows = append(summaryRows, summaryRow{apiName, endpointKey, len(groupResources), targetPresent})
	}

	totalRouted := 0
	for _, v := range groups {
		totalRouted += len(v)
	}
	fmt.Fprintf(outW, "\nProject: %s\nSource:  %s\nOutput:  %s\n\n", projectName, openAPIPath, outDir)
	fmt.Fprintf(outW, "%d API(s) generated from %d distinct backend endpoint(s), %d backend-routed resources.\n\n",
		len(groups), len(groups), totalRouted)
	fmt.Fprintf(outW, "%-45s %-30s %5s  %s\n", "API name", "Endpoint key", "#Res", "Target config")
	fmt.Fprintln(outW, strings.Repeat("-", 100))
	for _, row := range summaryRows {
		status := "present"
		if !row.present {
			status = "MISSING - get from I&M"
		}
		fmt.Fprintf(outW, "%-45s %-30s %5d  %s\n", row.apiName, row.endpointKey, row.count, status)
	}
	if len(unrouted) > 0 {
		var ids []string
		for _, r := range unrouted {
			ids = append(ids, r.ResourceID)
		}
		fmt.Fprintf(outW, "\nSkipped (no endpoint key, not backend-routed): %s\n", strings.Join(ids, ", "))
	}
	fmt.Fprintf(outW, "\nPolicies copied to %s:\n", policiesOutDir)
	var copiedList []string
	for pid := range policiesCopied {
		copiedList = append(copiedList, pid)
	}
	sort.Strings(copiedList)
	for _, pid := range copiedList {
		fmt.Fprintf(outW, "  - %s\n", pid)
	}
	if len(policiesMissing) > 0 {
		var missingList []string
		for pid := range policiesMissing {
			missingList = append(missingList, pid)
		}
		sort.Strings(missingList)
		fmt.Fprintf(errW, "\nWARNING: policy file(s) referenced but not found in %s/policies/:\n", projectDir)
		for _, pid := range missingList {
			fmt.Fprintf(errW, "  - %s\n", pid)
		}
	}
	fmt.Fprintf(outW, "\nCorrected master swagger written to: %s\n", correctedPath)
	fmt.Fprintf(outW, "Per-API swagger files written to:   %s/\n", splitDir)
	fmt.Fprintf(outW, "Log written to:                     %s\n", logPath)
	return nil
}

// ===========================================================================
// WSO2 Publisher REST client
// ===========================================================================

type apimClient struct {
	host       string
	httpClient *http.Client
	token      string
}

func newAPIMClient(host string) *apimClient {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	return &apimClient{host: host, httpClient: &http.Client{Transport: tr, Timeout: 60 * time.Second}}
}

func (c *apimClient) pub(path string) string {
	return fmt.Sprintf("https://%s:9443/api/am/publisher/v4%s", c.host, path)
}

func (c *apimClient) doJSON(method, url string, body interface{}, out interface{}) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s -> %d: %s", method, url, resp.StatusCode, string(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *apimClient) registerClient(adminUser, adminPass string) (clientID, clientSecret string, err error) {
	payload := map[string]interface{}{
		"clientName": "fiorano_apim_tool",
		"owner":      adminUser,
		"grantType":  "password refresh_token client_credentials",
		"saasApp":    true,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", fmt.Sprintf("https://%s:9443/client-registration/v0.17/register", c.host), bytes.NewReader(b))
	req.SetBasicAuth(adminUser, adminPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("register client -> %d: %s", resp.StatusCode, string(data))
	}
	var out struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", "", err
	}
	return out.ClientID, out.ClientSecret, nil
}

func (c *apimClient) getToken(clientID, clientSecret, adminUser, adminPass string) error {
	form := fmt.Sprintf(
		"grant_type=password&username=%s&password=%s&scope=apim%%3Aapi_view+apim%%3Aapi_create+apim%%3Aapi_manage+apim%%3Aapi_publish+apim%%3Asubscribe+apim%%3Aapp_manage",
		adminUser, adminPass)
	req, _ := http.NewRequest("POST", fmt.Sprintf("https://%s:9443/oauth2/token", c.host), strings.NewReader(form))
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("get token -> %d: %s", resp.StatusCode, string(data))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	c.token = out.AccessToken
	return nil
}

func (c *apimClient) importOpenAPI(swaggerFile string, additionalProperties map[string]interface{}) (apiID string, err error) {
	f, err := os.Open(swaggerFile)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filepath.Base(swaggerFile))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	apJSON, _ := json.Marshal(additionalProperties)
	if err := w.WriteField("additionalProperties", string(apJSON)); err != nil {
		return "", err
	}
	w.Close()

	req, err := http.NewRequest("POST", c.pub("/apis/import-openapi"), &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("import-openapi -> %d: %s", resp.StatusCode, string(data))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("import-openapi: no id in response: %s", string(data))
	}
	return out.ID, nil
}

// createRevision/deployRevision/publish work for both /apis and /api-products
// by taking the REST base segment ("apis" or "api-products") as a parameter.
func (c *apimClient) createRevision(base, id, description string) (revisionID string, err error) {
	var out struct {
		ID string `json:"id"`
	}
	url := c.pub(fmt.Sprintf("/%s/%s/revisions", base, id))
	err = c.doJSON("POST", url, map[string]string{"description": description}, &out)
	return out.ID, err
}

func (c *apimClient) deployRevision(base, id, revisionID, environment, vhost string) error {
	url := c.pub(fmt.Sprintf("/%s/%s/deploy-revision?revisionId=%s", base, id, revisionID))
	body := []map[string]interface{}{{"name": environment, "vhost": vhost, "displayOnDevportal": true}}
	var out interface{}
	return c.doJSON("POST", url, body, &out)
}

func (c *apimClient) publish(base, id string) error {
	path := "/apis/change-lifecycle"
	if base == "api-products" {
		path = "/api-products/change-lifecycle"
	}
	idParam := "apiId"
	if base == "api-products" {
		idParam = "apiProductId"
	}
	url := c.pub(fmt.Sprintf("%s?%s=%s&action=Publish", path, idParam, id))
	var out interface{}
	return c.doJSON("POST", url, nil, &out)
}

func (c *apimClient) getAPI(apiID string) (map[string]interface{}, error) {
	var out map[string]interface{}
	err := c.doJSON("GET", c.pub("/apis/"+apiID), nil, &out)
	return out, err
}

func (c *apimClient) updateAPI(apiID string, body map[string]interface{}) error {
	var out interface{}
	return c.doJSON("PUT", c.pub("/apis/"+apiID), body, &out)
}

func (c *apimClient) createProduct(body map[string]interface{}) (productID string, err error) {
	var out struct {
		ID string `json:"id"`
	}
	err = c.doJSON("POST", c.pub("/api-products"), body, &out)
	return out.ID, err
}

// ===========================================================================
// cmd: create-apis
// ===========================================================================

func loadOrInitConfig(configPath string) (*properties, error) {
	cfg, err := loadProperties(configPath)
	if err != nil {
		return nil, err
	}
	if cfg.Get("host", "") == "" || cfg.Get("adminUser", "") == "" {
		return nil, fmt.Errorf(
			"%s is missing required fields. Create it with at least:\n\n"+
				"  host=your-wso2-host\n  adminUser=admin\n  adminPassword=yourpassword\n  environment=Default\n  vhost=your-wso2-host\n",
			configPath)
	}
	return cfg, nil
}

func ensureClient(cfg *properties, configPath string, client *apimClient) error {
	clientID := cfg.Get("clientId", "")
	clientSecret := cfg.Get("clientSecret", "")
	if clientID == "" || clientSecret == "" {
		var err error
		clientID, clientSecret, err = client.registerClient(cfg.Get("adminUser", ""), cfg.Get("adminPassword", ""))
		if err != nil {
			return fmt.Errorf("registering REST client: %w", err)
		}
		cfg.Set("clientId", clientID)
		cfg.Set("clientSecret", clientSecret)
		if err := cfg.Save(configPath); err != nil {
			return fmt.Errorf("saving %s: %w", configPath, err)
		}
		fmt.Println("Registered a new REST client and saved it to", configPath)
	}
	return client.getToken(clientID, clientSecret, cfg.Get("adminUser", ""), cfg.Get("adminPassword", ""))
}

// splitFileGroups returns, for each generated split-by-endpoint file matching
// "<projectName>-<key>.yaml", its key and full path, sorted by key.
func splitFileGroups(splitDir, projectName string) (map[string]string, error) {
	entries, err := os.ReadDir(splitDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", splitDir, err)
	}
	prefix := projectName + "-"
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		key := strings.TrimSuffix(strings.TrimPrefix(e.Name(), prefix), ".yaml")
		out[key] = filepath.Join(splitDir, e.Name())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no \"%s<key>.yaml\" files found in %s — run \"split\" first", prefix, splitDir)
	}
	return out, nil
}

func cmdCreateAPIs(args []string) error {
	fs := flag.NewFlagSet("create-apis", flag.ExitOnError)
	configFlag := fs.String("config", resolveConfPath("config.properties"), "path to config.properties (default: conf/config.properties next to this binary)")
	backendsFlag := fs.String("backends", "", "path to backends.properties (or backendsFile= in config.properties; default backends.properties)")
	splitDirFlag := fs.String("split-dir", "", "the split-by-endpoint directory from \"split\" (or splitDir= in config.properties)")
	projectNameFlag := fs.String("project-name", "", "project name, as used in the generated filenames (or projectName= in config.properties)")
	versionFlag := fs.String("api-version", "", "version for every generated API (or apiVersion= in config.properties; default 1.0)")
	mockBaseFlag := fs.String("mock-base-url", "", "if set, APIs with no backend yet get <mock-base-url>/<endpoint-key> instead of a placeholder (or mockBaseUrl= in config.properties)")
	fs.Parse(reorderArgs(args))

	cfg, err := loadOrInitConfig(*configFlag)
	if err != nil {
		return err
	}

	if *splitDirFlag == "" {
		*splitDirFlag = cfg.Get("splitDir", "")
	}
	if *projectNameFlag == "" {
		*projectNameFlag = cfg.Get("projectName", "")
	}
	if *versionFlag == "" {
		*versionFlag = cfg.Get("apiVersion", "1.0")
	}
	if *mockBaseFlag == "" {
		*mockBaseFlag = cfg.Get("mockBaseUrl", "")
	}
	if *backendsFlag == "" {
		*backendsFlag = resolveConfPath(cfg.Get("backendsFile", "backends.properties"))
	}

	if *splitDirFlag == "" || *projectNameFlag == "" {
		return fmt.Errorf("--split-dir and --project-name are required (or set splitDir= / projectName= in %s)", *configFlag)
	}

	backends, err := loadProperties(*backendsFlag)
	if err != nil {
		return err
	}

	groups, err := splitFileGroups(*splitDirFlag, *projectNameFlag)
	if err != nil {
		return err
	}

	client := newAPIMClient(cfg.Get("host", ""))
	if err := ensureClient(cfg, *configFlag, client); err != nil {
		return err
	}

	environment := cfg.Get("environment", "Default")
	vhost := cfg.Get("vhost", cfg.Get("host", ""))

	outW, _, closeLog, logPath, err := openProjectLog(*projectNameFlag, "create-apis")
	if err != nil {
		return err
	}
	defer closeLog()

	var keys []string
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintf(outW, "%-45s %-10s %s\n", "API name", "Status", "Backend")
	fmt.Fprintln(outW, strings.Repeat("-", 100))

	for _, key := range keys {
		entry := getBackendEntry(backends, key)
		name := *projectNameFlag + "-" + key
		context := "/" + *projectNameFlag + "/" + strings.ToLower(key)
		backend := entry.Backend
		if backend == "" {
			if *mockBaseFlag != "" {
				backend = strings.TrimRight(*mockBaseFlag, "/") + "/" + key
			} else {
				backend = "https://CHANGE-ME/" + key
			}
		}

		additionalProps := map[string]interface{}{
			"name":     name,
			"version":  *versionFlag,
			"context":  context,
			"policies": []string{"Unlimited"},
			"endpointConfig": map[string]interface{}{
				"endpoint_type": "http",
				"production_endpoints": map[string]string{
					"url": backend,
				},
			},
		}

		apiID, err := client.importOpenAPI(groups[key], additionalProps)
		if err != nil {
			fmt.Fprintf(outW, "%-45s %-10s %s\n", name, "FAILED", err.Error())
			continue
		}
		revID, err := client.createRevision("apis", apiID, "Imported from Fiorano")
		if err != nil {
			fmt.Fprintf(outW, "%-45s %-10s %s\n", name, "FAILED", "revision: "+err.Error())
			continue
		}
		if err := client.deployRevision("apis", apiID, revID, environment, vhost); err != nil {
			fmt.Fprintf(outW, "%-45s %-10s %s\n", name, "FAILED", "deploy: "+err.Error())
			continue
		}
		if err := client.publish("apis", apiID); err != nil {
			fmt.Fprintf(outW, "%-45s %-10s %s\n", name, "FAILED", "publish: "+err.Error())
			continue
		}

		setBackendEntry(backends, backendEntry{Key: key, Name: name, Context: context, ID: apiID, Backend: backend})
		fmt.Fprintf(outW, "%-45s %-10s %s\n", name, "OK", backend)
	}

	if err := backends.Save(*backendsFlag); err != nil {
		return fmt.Errorf("saving %s: %w", *backendsFlag, err)
	}
	fmt.Fprintln(outW, "\nSaved API ids and backends to", *backendsFlag)
	fmt.Fprintln(outW, "Log written to:", logPath)
	return nil
}

// ===========================================================================
// cmd: create-product
// ===========================================================================

func prompt(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func cmdCreateProduct(args []string) error {
	fs := flag.NewFlagSet("create-product", flag.ExitOnError)
	configFlag := fs.String("config", resolveConfPath("config.properties"), "path to config.properties (default: conf/config.properties next to this binary)")
	backendsFlag := fs.String("backends", "", "path to backends.properties (or backendsFile= in config.properties; default backends.properties)")
	splitDirFlag := fs.String("split-dir", "", "the split-by-endpoint directory from \"split\" (or splitDir= in config.properties)")
	projectNameFlag := fs.String("project-name", "", "project name, as used in the generated filenames (or projectName= in config.properties)")
	nameFlag := fs.String("product-name", "", "API Product name (or productName= in config.properties; prompted if neither is given)")
	contextFlag := fs.String("product-context", "", "API Product context (or productContext= in config.properties; default /<product-name>)")
	versionFlag := fs.String("product-version", "", "API Product version (or productVersion= in config.properties; default 1.0)")
	fs.Parse(reorderArgs(args))

	cfg, err := loadOrInitConfig(*configFlag)
	if err != nil {
		return err
	}

	if *splitDirFlag == "" {
		*splitDirFlag = cfg.Get("splitDir", "")
	}
	if *projectNameFlag == "" {
		*projectNameFlag = cfg.Get("projectName", "")
	}
	if *nameFlag == "" {
		*nameFlag = cfg.Get("productName", "")
	}
	if *contextFlag == "" {
		*contextFlag = cfg.Get("productContext", "")
	}
	if *versionFlag == "" {
		*versionFlag = cfg.Get("productVersion", "1.0")
	}
	if *backendsFlag == "" {
		*backendsFlag = resolveConfPath(cfg.Get("backendsFile", "backends.properties"))
	}

	if *splitDirFlag == "" || *projectNameFlag == "" {
		return fmt.Errorf("--split-dir and --project-name are required (or set splitDir= / projectName= in %s)", *configFlag)
	}

	backends, err := loadProperties(*backendsFlag)
	if err != nil {
		return err
	}

	productName := *nameFlag
	if productName == "" {
		productName = prompt("API Product name", *projectNameFlag)
	}
	productContext := *contextFlag
	if productContext == "" {
		productContext = "/" + productName
	}

	outW, _, closeLog, logPath, err := openProjectLog(*projectNameFlag, "create-product")
	if err != nil {
		return err
	}
	defer closeLog()

	keys := backendKeys(backends)
	if len(keys) == 0 {
		return fmt.Errorf("no APIs found in %s — run create-apis first", *backendsFlag)
	}

	var apiRefs []map[string]interface{}
	for _, key := range keys {
		e := getBackendEntry(backends, key)
		if e.ID == "" {
			fmt.Fprintf(outW, "  skipping %s: no API id yet (run create-apis)\n", key)
			continue
		}
		swaggerFile := filepath.Join(*splitDirFlag, e.Name+".yaml")
		raw, err := os.ReadFile(swaggerFile)
		if err != nil {
			fmt.Fprintf(outW, "  WARNING: couldn't read %s for operations, skipping its operations list: %v\n", swaggerFile, err)
			apiRefs = append(apiRefs, map[string]interface{}{"name": e.Name, "apiId": e.ID, "version": "1.0", "operations": []interface{}{}})
			continue
		}
		fixed := fixOpenAPIText(string(raw))
		_, blocks, _, err := splitOpenAPISections(fixed)
		if err != nil {
			return err
		}
		var paths []string
		for p := range blocks {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		ops := pathMethods(blocks, paths)
		var opsOut []map[string]string
		for _, op := range ops {
			opsOut = append(opsOut, map[string]string{"target": op[0], "verb": op[1]})
		}
		apiRefs = append(apiRefs, map[string]interface{}{"name": e.Name, "apiId": e.ID, "version": "1.0", "operations": opsOut})
	}

	payload := map[string]interface{}{
		"name":         productName,
		"context":      productContext,
		"version":      *versionFlag,
		"policies":     []string{"Unlimited"},
		"gatewayVendor": "wso2",
		"apis":         apiRefs,
	}

	client := newAPIMClient(cfg.Get("host", ""))
	if err := ensureClient(cfg, *configFlag, client); err != nil {
		return err
	}

	productID, err := client.createProduct(payload)
	if err != nil {
		return fmt.Errorf("creating product: %w", err)
	}
	fmt.Fprintln(outW, "Created product:", productName, "id:", productID)

	revID, err := client.createRevision("api-products", productID, "Initial revision")
	if err != nil {
		return fmt.Errorf("product revision: %w", err)
	}
	environment := cfg.Get("environment", "Default")
	vhost := cfg.Get("vhost", cfg.Get("host", ""))
	if err := client.deployRevision("api-products", productID, revID, environment, vhost); err != nil {
		return fmt.Errorf("product deploy: %w", err)
	}
	if err := client.publish("api-products", productID); err != nil {
		return fmt.Errorf("product publish: %w", err)
	}

	backends.Set("product.name", productName)
	backends.Set("product.context", productContext)
	backends.Set("product.id", productID)
	if err := backends.Save(*backendsFlag); err != nil {
		return err
	}

	fmt.Fprintf(outW, "\nProduct %s published at context %s (combines %d APIs)\n", productName, productContext, len(apiRefs))
	fmt.Fprintln(outW, "Log written to:", logPath)
	return nil
}

// ===========================================================================
// cmd: update-backends
// ===========================================================================

func cmdUpdateBackends(args []string) error {
	fs := flag.NewFlagSet("update-backends", flag.ExitOnError)
	configFlag := fs.String("config", resolveConfPath("config.properties"), "path to config.properties (default: conf/config.properties next to this binary)")
	backendsFlag := fs.String("backends", "", "path to backends.properties (or backendsFile= in config.properties; default backends.properties)")
	fs.Parse(reorderArgs(args))

	cfg, err := loadOrInitConfig(*configFlag)
	if err != nil {
		return err
	}
	if *backendsFlag == "" {
		*backendsFlag = resolveConfPath(cfg.Get("backendsFile", "backends.properties"))
	}
	backends, err := loadProperties(*backendsFlag)
	if err != nil {
		return err
	}

	client := newAPIMClient(cfg.Get("host", ""))
	if err := ensureClient(cfg, *configFlag, client); err != nil {
		return err
	}
	environment := cfg.Get("environment", "Default")
	vhost := cfg.Get("vhost", cfg.Get("host", ""))

	outW, _, closeLog, logPath, err := openProjectLog(cfg.Get("projectName", "project"), "update-backends")
	if err != nil {
		return err
	}
	defer closeLog()

	keys := backendKeys(backends)
	var updated, upToDate, pending, failed []string

	for _, key := range keys {
		e := getBackendEntry(backends, key)
		if e.ID == "" {
			continue
		}
		if e.Backend == "" || strings.Contains(e.Backend, "CHANGE-ME") {
			pending = append(pending, e.Name)
			continue
		}

		api, err := client.getAPI(e.ID)
		if err != nil {
			fmt.Fprintf(outW, "  %s: FAILED to fetch: %v\n", e.Name, err)
			failed = append(failed, e.Name)
			continue
		}
		endpointConfig, _ := api["endpointConfig"].(map[string]interface{})
		currentURL := ""
		if endpointConfig != nil {
			if prod, ok := endpointConfig["production_endpoints"].(map[string]interface{}); ok {
				currentURL, _ = prod["url"].(string)
			}
		}
		if currentURL == e.Backend {
			upToDate = append(upToDate, e.Name)
			continue
		}

		if endpointConfig == nil {
			endpointConfig = map[string]interface{}{"endpoint_type": "http"}
		}
		endpointConfig["production_endpoints"] = map[string]string{"url": e.Backend}
		api["endpointConfig"] = endpointConfig

		if err := client.updateAPI(e.ID, api); err != nil {
			fmt.Fprintf(outW, "  %s: FAILED to update: %v\n", e.Name, err)
			failed = append(failed, e.Name)
			continue
		}
		revID, err := client.createRevision("apis", e.ID, "Backend address updated")
		if err != nil {
			fmt.Fprintf(outW, "  %s: updated but FAILED to create revision: %v\n", e.Name, err)
			failed = append(failed, e.Name)
			continue
		}
		if err := client.deployRevision("apis", e.ID, revID, environment, vhost); err != nil {
			fmt.Fprintf(outW, "  %s: updated but FAILED to deploy: %v\n", e.Name, err)
			failed = append(failed, e.Name)
			continue
		}
		fmt.Fprintf(outW, "  %s: updated -> %s (new revision deployed)\n", e.Name, e.Backend)
		updated = append(updated, e.Name)
	}

	fmt.Fprintf(outW, "\n%d updated, %d already up to date, %d pending (no backend set yet), %d failed\n",
		len(updated), len(upToDate), len(pending), len(failed))
	if len(pending) > 0 {
		fmt.Fprintln(outW, "\nStill waiting on a backend address for:")
		for _, n := range pending {
			fmt.Fprintln(outW, "  -", n)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintln(outW, "\nFailed (see errors above):")
		for _, n := range failed {
			fmt.Fprintln(outW, "  -", n)
		}
	}
	fmt.Fprintln(outW, "\nLog written to:", logPath)
	return nil
}

// ===========================================================================
// main
// ===========================================================================

func usage() {
	fmt.Fprintln(os.Stderr, `fiorano-to-wso2-apim-migrator — migrate a Fiorano REST API export into WSO2 API Manager

Folder layout (run the binary from inside bin/ — the tool finds conf/
automatically next to it, no matter what directory you're in):

  <tool root>/
    bin/
      fiorano-to-wso2-apim-migrator-<platform>      <- run this
    conf/
      config.properties                 <- edit this once
      backends.properties                <- created and updated by the tool
    logs/
      <projectName>.log                 <- created automatically; every run of
                                            every subcommand for this project
                                            appends here (created if missing)

Usage (every value below can also come from config.properties — see below — so
most days you just run the bare subcommand with no flags at all):
  fiorano-to-wso2-apim-migrator split            [<fiorano_project_dir>] [--out dir] [--version X.Y] [--config FILE]
  fiorano-to-wso2-apim-migrator create-apis      [--config FILE] [--backends FILE] [--split-dir DIR] [--project-name NAME] [--mock-base-url URL] [--api-version X.Y]
  fiorano-to-wso2-apim-migrator create-product   [--config FILE] [--backends FILE] [--split-dir DIR] [--project-name NAME] [--product-name NAME] [--product-context PATH] [--product-version X.Y]
  fiorano-to-wso2-apim-migrator update-backends  [--config FILE] [--backends FILE]

A flag you pass always wins; config.properties only fills in what you leave out.

config.properties — full schema, with every section commented:

  # --- Server connection (required) ---
  host=your-wso2-host
  adminUser=admin
  adminPassword=yourpassword
  environment=Default
  vhost=your-wso2-host

  # --- Project settings (used by split, create-apis, create-product) ---
  projectDir=/path/to/YourProjectName__1.0
  outDir=/path/to/output
  splitDir=/path/to/output/split-by-endpoint
  projectName=YourProjectName
  backendsFile=backends.properties
  apiVersion=1.0
  mockBaseUrl=http://localhost:8290/mock/cbs

  # --- API Product settings (used by create-product) ---
  productName=YourProjectName
  productContext=/YourProjectName
  productVersion=1.0

  # --- Added automatically after the first run — leave these alone ---
  clientId=
  clientSecret=

With all of the above filled in, the four commands become just:
  fiorano-to-wso2-apim-migrator split
  fiorano-to-wso2-apim-migrator create-apis
  fiorano-to-wso2-apim-migrator create-product
  fiorano-to-wso2-apim-migrator update-backends

backends.properties (created and updated by the tool itself — holds no secrets):
  api.<key>.name=...
  api.<key>.context=...
  api.<key>.id=...
  api.<key>.backend=...   (leave blank until you have the real address; update-backends skips blanks)
  product.name / product.context / product.id   (added by create-product)

logs/<projectName>.log is created automatically the first time any subcommand
runs for that project, and every later run — by any subcommand — appends to
the same file rather than overwriting it, so it builds a full history.`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	sub := os.Args[1]
	rest := os.Args[2:]

	var err error
	switch sub {
	case "split":
		err = cmdSplit(rest)
	case "create-apis":
		err = cmdCreateAPIs(rest)
	case "create-product":
		err = cmdCreateProduct(rest)
	case "update-backends":
		err = cmdUpdateBackends(rest)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
