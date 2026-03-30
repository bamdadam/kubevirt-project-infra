package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	pi_github "kubevirt.io/project-infra/pkg/github"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/prow/pkg/github"
)

var log *logrus.Logger

func init() {
	log = logrus.New()
	log.SetOutput(os.Stdout)
}

type GitHubEvent struct {
	Type    string
	GUID    string
	Payload []byte
}

type githubClientInterface interface {
	GetPullRequest(string, string, int) (*github.PullRequest, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
}

type ConformanceDetectorInterface interface {
	HasConformanceTestsChanged(fileContents map[string]string, changedLines map[string][]int) (bool, error)
}

type GitHubEventsHandler struct {
	eventsChan         <-chan *GitHubEvent
	logger             *logrus.Logger
	ghClient           githubClientInterface
	conformanceChecker ConformanceDetectorInterface
}

func NewGitHubEventsHandler(
	eventsChan <-chan *GitHubEvent,
	logger *logrus.Logger,
	ghClient githubClientInterface,
	conformanceChecker ConformanceDetectorInterface) *GitHubEventsHandler {

	return &GitHubEventsHandler{
		eventsChan:         eventsChan,
		logger:             logger,
		ghClient:           ghClient,
		conformanceChecker: conformanceChecker,
	}
}

func (h *GitHubEventsHandler) Handle(incomingEvent *GitHubEvent) {
	h.logger.Infoln("GitHub events handler started")
	eventLog := h.logger.WithField("event-guid", incomingEvent.GUID)
	switch incomingEvent.Type {
	case "pull_request":
		eventLog.Infoln("Handling pull request event")
		var event github.PullRequestEvent
		if err := json.Unmarshal(incomingEvent.Payload, &event); err != nil {
			eventLog.WithError(err).Error("Could not unmarshal event.")
			return
		}
		h.handlePullRequestEvent(eventLog, &event)
	default:
		eventLog.Infoln("Dropping irrelevant:", incomingEvent.Type)
	}
}

func (h *GitHubEventsHandler) handlePullRequestEvent(eventLog *logrus.Entry, event *github.PullRequestEvent) {
	eventLog.Infof("Handling pull request: %s [%d]", event.Repo.FullName, event.PullRequest.Number)

	if event.Action != github.PullRequestActionOpened && event.Action != github.PullRequestActionSynchronize {
		eventLog.Debugf("Skipping action: %s", event.Action)
		return
	}

	org, repo, err := pi_github.OrgRepo(event.Repo.FullName)
	if err != nil {
		eventLog.WithError(err).Errorf("Could not parse org/repo from event")
		return
	}

	pr, err := h.ghClient.GetPullRequest(org, repo, event.PullRequest.Number)
	if err != nil {
		eventLog.WithError(err).Errorf("Could not get PR")
		return
	}

	// Get changed files
	changes, err := h.ghClient.GetPullRequestChanges(org, repo, pr.Number)
	if err != nil {
		eventLog.WithError(err).Errorf("Could not get PR changes")
		return
	}

	eventLog.Debugf("Found %d changed files", len(changes))

	// Extract test files and their changes
	fileContents, changedLines, err := h.extractTestFileChanges(eventLog, org, repo, pr, changes)
	if err != nil {
		eventLog.WithError(err).Errorf("Could not extract test file changes")
		return
	}

	if len(fileContents) == 0 {
		eventLog.Info("No test files changed")
		return
	}

	// Check for Conformance test changes
	hasConformanceChanges, err := h.conformanceChecker.HasConformanceTestsChanged(fileContents, changedLines)
	if err != nil {
		eventLog.WithError(err).Errorf("Error checking for Conformance test changes")
		return
	}

	if hasConformanceChanges {
		eventLog.Info("Conformance tests have been changed in this PR")
	} else {
		eventLog.Info("No Conformance test changes detected")
	}
}

// extractTestFileChanges extracts test file changes from PR changes
func (h *GitHubEventsHandler) extractTestFileChanges(
	eventLog *logrus.Entry,
	org, repo string,
	pr *github.PullRequest,
	changes []github.PullRequestChange) (map[string]string, map[string][]int, error) {

	fileContents := make(map[string]string)
	changedLines := make(map[string][]int)

	for _, change := range changes {
		// Only process test files
		if !h.isTestFile(change.Filename) {
			continue
		}

		eventLog.Debugf("Processing test file: %s", change.Filename)

		// Skip deleted files
		if change.Status == "deleted" {
			eventLog.Debugf("Skipping deleted file: %s", change.Filename)
			continue
		}

		// For now, we work with the patch content directly
		// The patch contains the diff which we can use to extract changed lines
		if change.Patch == "" {
			eventLog.Debugf("No patch content for file: %s", change.Filename)
			continue
		}

		// Extract changed lines from patch
		lines := h.extractChangedLines(change.Patch)
		if len(lines) == 0 {
			eventLog.Debugf("No changed lines found for file: %s", change.Filename)
			continue
		}

		changedLines[change.Filename] = lines
		// Store patch content as file content for now (we'll need full content later)
		fileContents[change.Filename] = change.Patch

		eventLog.Debugf("File %s has %d changed lines", change.Filename, len(lines))
	}

	return fileContents, changedLines, nil
}

// isTestFile checks if a file is a test file
func (h *GitHubEventsHandler) isTestFile(filename string) bool {
	// Check for *_test.go files
	if strings.HasSuffix(filename, "_test.go") {
		return true
	}

	// Check for files in /tests/ directory
	if strings.Contains(filename, "/tests/") {
		return true
	}

	return false
}

// extractChangedLines extracts line numbers from a patch
func (h *GitHubEventsHandler) extractChangedLines(patch string) []int {
	var lines []int
	var currentLineNo int

	patchLines := strings.Split(patch, "\n")
	for _, patchLine := range patchLines {
		// Check for hunk header (@@)
		if strings.HasPrefix(patchLine, "@@") {
			// Parse the line number from hunk header
			// Format: @@ -oldStart,oldCount +newStart,newCount @@
			parts := strings.Fields(patchLine)
			if len(parts) >= 3 {
				// Get the new file line number
				newPart := parts[2]
				// Remove the leading '+'
				newPart = strings.TrimPrefix(newPart, "+")
				// Remove trailing comma if present
				newPart = strings.Split(newPart, ",")[0]
				// Parse the starting line number
				if _, err := fmt.Sscanf(newPart, "%d", &currentLineNo); err != nil {
					currentLineNo = 0
				}
			}
			continue
		}

		if currentLineNo == 0 {
			continue
		}

		// Lines starting with + are additions, - are deletions, space means context
		if strings.HasPrefix(patchLine, "+") && !strings.HasPrefix(patchLine, "+++") {
			lines = append(lines, currentLineNo)
			currentLineNo++
		} else if strings.HasPrefix(patchLine, "-") && !strings.HasPrefix(patchLine, "---") {
			// Deletions don't increment the line counter for the new file
		} else if patchLine != "" || strings.HasPrefix(patchLine, " ") {
			// Context lines (start with space or are empty in the new file)
			currentLineNo++
		} else {
			currentLineNo++
		}
	}

	return lines
}

// ServeHTTP implements http.Handler interface for the event handler
func (h *GitHubEventsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.WithError(err).Error("Failed to read request body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	event := &GitHubEvent{
		Type:    r.Header.Get("X-GitHub-Event"),
		GUID:    r.Header.Get("X-GitHub-Delivery"),
		Payload: body,
	}

	h.Handle(event)
	w.WriteHeader(http.StatusOK)
}
