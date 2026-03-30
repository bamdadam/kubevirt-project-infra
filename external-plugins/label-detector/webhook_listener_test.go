package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
	"kubevirt.io/project-infra/pkg/ginkgo"
	gitv2 "sigs.k8s.io/prow/pkg/git/v2"
	"sigs.k8s.io/prow/pkg/github"
)

var sep = strings.Repeat("=", 80)

func TestWebhookListener(t *testing.T) {
	port := 8888
	addr := fmt.Sprintf(":%d", port)
	eventReceived := make(chan bool, 1)

	// Initialize git client factory for sparse checkouts
	gitClientFactory, err := gitv2.NewClientFactory()
	if err != nil {
		t.Fatalf("Failed to create git client factory: %v", err)
	}

	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			fmt.Printf("Failed to read body: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body.Close()

		fmt.Println("\n" + sep)
		fmt.Println("RECEIVED GITHUB WEBHOOK")
		fmt.Println(sep)
		fmt.Println("\nRAW JSON PAYLOAD:")
		fmt.Println(string(body))

		var event github.PullRequestEvent
		err = json.Unmarshal(body, &event)
		if err != nil {
			fmt.Printf("Failed to unmarshal as PullRequestEvent: %v\n", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if event.Action != github.PullRequestActionOpened && event.Action != github.PullRequestActionSynchronize {
			fmt.Printf("Skipping action: %s (only interested in 'opened' or 'synchronize')\n", event.Action)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Skipped"))
			return
		}

		fmt.Println("\n" + sep)
		fmt.Println("PARSED STRUCTURE")
		fmt.Println(sep)
		prettyEvent, _ := json.MarshalIndent(event, "", "  ")
		fmt.Println(string(prettyEvent))

		fmt.Println("\n" + sep)
		fmt.Println("KEY FIELDS")
		fmt.Println(sep)
		fmt.Printf("Action:          %s\n", event.Action)
		fmt.Printf("PR Number:       %d\n", event.PullRequest.Number)
		fmt.Printf("PR State:        %s\n", event.PullRequest.State)
		fmt.Printf("PR Title:        %s\n", event.PullRequest.Title)
		fmt.Printf("Base Ref:        %s\n", event.PullRequest.Base.Ref)
		fmt.Printf("Head Ref:        %s\n", event.PullRequest.Head.Ref)
		fmt.Printf("Repo:            %s\n", event.Repo.FullName)
		fmt.Printf("Label (if any):  %s\n", event.Label.Name)

		org := event.Repo.Owner.Login
		repo := event.Repo.Name

		baseSHA := event.PullRequest.Base.SHA
		headSHA := event.PullRequest.Head.SHA

		fmt.Println("\n" + sep)
		fmt.Println("CLONING REPO")
		fmt.Println(sep)

		repoClient, err := gitClientFactory.ClientFor(org, repo)
		if err != nil {
			fmt.Printf("Failed to create repo client: %v\n", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		repoDir := filepath.Join(repoClient.Directory(), "tests")

		fmt.Println("\n" + sep)
		fmt.Println("CONFORMANCE TEST DETECTION VIA DRY RUN")
		fmt.Println(sep)

		changedFiles := getChangedGoFiles(org, repo, event.PullRequest.Number)

		// Convert to absolute paths for matching with DryRun output
		absChangedFiles := make(map[string]bool)
		for relPath := range changedFiles {
			absPath := filepath.Join(repoDir, relPath)
			absChangedFiles[absPath] = true
		}
		changedFiles = absChangedFiles

		// Checkout base and run DryRun
		if err := repoClient.Checkout(baseSHA); err != nil {
			fmt.Printf("Failed to checkout base SHA: %v\n", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		baseSpecs, _ := getConformanceSpecs(repoDir)
		baseSpecs = filterSpecsByChangedFiles(baseSpecs, changedFiles)
		baseOutlines := parseChangedTestFiles(repoDir, changedFiles)
		baseSourceFiles := readChangedSourceFiles(repoDir, changedFiles)

		baseOutlineNodeMaps := make(map[string]map[string]*ginkgo.Node)
		for filename, nodes := range baseOutlines {
			baseOutlineNodeMaps[filename] = make(map[string]*ginkgo.Node)
			buildOutlineNodeMap(nodes, nil, baseOutlineNodeMaps[filename])
		}

		fmt.Printf("Base conformance specs: %d\n", len(baseSpecs))

		// Checkout head and run DryRun
		if err := repoClient.Checkout(headSHA); err != nil {
			fmt.Printf("Failed to checkout head SHA: %v\n", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		headSpecs, err := getConformanceSpecs(repoDir)
		if err != nil {
			fmt.Printf("Failed to get conformance specs: %v\n", err)
		}
		headSpecs = filterSpecsByChangedFiles(headSpecs, changedFiles)
		headOutlines := parseChangedTestFiles(repoDir, changedFiles)
		headSourceFiles := readChangedSourceFiles(repoDir, changedFiles)

		headOutlineNodeMaps := make(map[string]map[string]*ginkgo.Node)
		for filename, nodes := range headOutlines {
			headOutlineNodeMaps[filename] = make(map[string]*ginkgo.Node)
			buildOutlineNodeMap(nodes, nil, headOutlineNodeMaps[filename])
		}

		fmt.Printf("Head conformance specs: %d\n", len(headSpecs))

		baseSpecMap := specToMap(baseSpecs)
		headSpecMap := specToMap(headSpecs)
		changed := false

		// Check for ADDED and MODIFIED
		for name, headSpec := range headSpecMap {
			if baseSpec, exists := baseSpecMap[name]; !exists {
				fmt.Printf("[ADDED]    %s\n", name)
				changed = true
			} else {
				// Both have this spec - check if MODIFIED
				if isSpecModified(baseSpec, headSpec, baseSourceFiles, headSourceFiles, baseOutlineNodeMaps, headOutlineNodeMaps) {
					fmt.Printf("[MODIFIED] %s\n", name)
					changed = true
				}
			}
		}

		// Check for REMOVED
		for name := range baseSpecMap {
			if _, exists := headSpecMap[name]; !exists {
				fmt.Printf("[REMOVED]  %s\n", name)
				changed = true
			}
		}

		if changed {
			fmt.Println("\nRESULT: Conformance tests changed")
		} else {
			fmt.Println("\nRESULT: No Conformance test changes detected")
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))

		select {
		case eventReceived <- true:
		default:
		}
	})

	server := &http.Server{Addr: addr}
	go func() {
		fmt.Printf("\nWebhook listener starting on http://localhost%s/webhook\n", addr)
		fmt.Println("Configure your GitHub webhook to point here and trigger a PR event!")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Server error: %v\n", err)
		}
	}()

	select {
	case <-eventReceived:
		fmt.Println("\nEvent received and logged")
	case <-time.After(10 * time.Minute):
		fmt.Println("\nTimeout: No events received in 10 minutes")
	}

	server.Close()
}

func getConformanceSpecs(testDir string) ([]types.SpecReport, error) {
	reports, _, err := ginkgo.DryRun(testDir)
	if err != nil {
		return nil, fmt.Errorf("DryRun failed on %s: %w", testDir, err)
	}
	return ginkgo.FilterSpecReports(reports, func(r types.SpecReport) bool {
		matched, _ := r.MatchesLabelFilter("Conformance")
		return matched
	}, -1), nil
}

func filterSpecsByChangedFiles(specs []types.SpecReport, changedFiles map[string]bool) []types.SpecReport {
	var filtered []types.SpecReport
	for _, spec := range specs {
		if changedFiles[spec.LeafNodeLocation.FileName] {
			filtered = append(filtered, spec)
		}
	}
	return filtered
}

func buildOutlineNodeMap(nodes []*ginkgo.Node, ancestors []string, m map[string]*ginkgo.Node) {
	for _, node := range nodes {
		currentPath := append(ancestors, node.Text)
		pathStr := strings.Join(currentPath, " ")

		if node.Spec {
			m[pathStr] = node
		}
		if len(node.Nodes) > 0 {
			buildOutlineNodeMap(node.Nodes, currentPath, m)
		}
	}
}

func specToMap(specs []types.SpecReport) map[string]types.SpecReport {
	m := make(map[string]types.SpecReport)
	for _, spec := range specs {
		m[spec.FullText()] = spec
	}
	return m
}

func getChangedGoFiles(org, repo string, prNumber int) map[string]bool {
	changedFiles := make(map[string]bool)
	changesURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d/files", org, repo, prNumber)

	resp, err := http.Get(changesURL)
	if err != nil {
		fmt.Printf("Failed to fetch PR changes: %v\n", err)
		return changedFiles
	}
	defer resp.Body.Close()

	var changes []github.PullRequestChange
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &changes)

	for _, change := range changes {
		if strings.HasSuffix(change.Filename, ".go") {
			changedFiles[change.Filename] = true
		}
	}

	return changedFiles
}

func parseChangedTestFiles(testDir string, changedFiles map[string]bool) map[string][]*ginkgo.Node {
	outlines := make(map[string][]*ginkgo.Node)

	for changedFile := range changedFiles {
		fullPath := filepath.Join(testDir, changedFile)
		nodes, err := ginkgo.OutlineFromFile(fullPath)
		if err == nil {
			outlines[fullPath] = nodes
		}
	}

	return outlines
}

func readChangedSourceFiles(testDir string, changedFiles map[string]bool) map[string][]byte {
	sources := make(map[string][]byte)

	for changedFile := range changedFiles {
		fullPath := filepath.Join(testDir, changedFile)
		content, err := os.ReadFile(fullPath)
		if err == nil {
			sources[fullPath] = content
		}
	}

	return sources
}

func isSpecModified(baseSpec, headSpec types.SpecReport, baseSourceFiles, headSourceFiles map[string][]byte, baseOutlineNodeMaps, headOutlineNodeMaps map[string]map[string]*ginkgo.Node) bool {
	baseHash := getSourceHashForSpec(baseSpec, baseSourceFiles, baseOutlineNodeMaps)
	headHash := getSourceHashForSpec(headSpec, headSourceFiles, headOutlineNodeMaps)

	if baseHash != headHash {
		return true
	}

	if !labelsEqual(baseSpec.Labels(), headSpec.Labels()) {
		return true
	}

	return false
}

func getSourceHashForSpec(spec types.SpecReport, sourceFiles map[string][]byte, outlineNodeMaps map[string]map[string]*ginkgo.Node) string {
	filename := spec.LeafNodeLocation.FileName
	source, exists := sourceFiles[filename]
	if !exists {
		return ""
	}

	outlineNodeMap, exists := outlineNodeMaps[filename]
	if !exists {
		return ""
	}

	node, exists := outlineNodeMap[spec.FullText()]
	if !exists || node.Start < 0 || node.End > len(source) {
		return ""
	}

	nodeCode := source[node.Start:node.End]
	hash := md5.Sum(nodeCode)
	return fmt.Sprintf("%x", hash)
}

func labelsEqual(base, head []string) bool {
	if len(base) != len(head) {
		return false
	}
	baseMap := make(map[string]bool)
	for _, l := range base {
		baseMap[l] = true
	}
	for _, l := range head {
		if !baseMap[l] {
			return false
		}
	}
	return true
}
