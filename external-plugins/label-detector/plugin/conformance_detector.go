/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package main

import (
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
)

// ConformanceDetector detects if Conformance tests have been changed in a PR
type ConformanceDetector struct {
	logger *logrus.Entry
}

// NewConformanceDetector creates a new ConformanceDetector
func NewConformanceDetector(logger *logrus.Entry) *ConformanceDetector {
	return &ConformanceDetector{
		logger: logger,
	}
}

// HasConformanceTestsChanged checks if any changed lines in test files contain Conformance tests
func (cd *ConformanceDetector) HasConformanceTestsChanged(fileContents map[string]string, changedLines map[string][]int) (bool, error) {
	for filename, content := range fileContents {
		changedLineNumbers, ok := changedLines[filename]
		if !ok || len(changedLineNumbers) == 0 {
			cd.logger.Debugf("No changed lines for %s", filename)
			continue
		}

		cd.logger.Debugf("Analyzing file: %s with changed lines: %v", filename, changedLineNumbers)

		// Check if any changed lines contain Conformance patterns
		if cd.hasConformanceInChangedLines(content, changedLineNumbers) {
			cd.logger.Infof("Found Conformance test changes in file: %s", filename)
			return true, nil
		}
	}

	cd.logger.Info("No Conformance test changes detected")
	return false, nil
}

// hasConformanceInChangedLines checks if any of the changed lines contain Conformance patterns
func (cd *ConformanceDetector) hasConformanceInChangedLines(content string, changedLines []int) bool {
	lines := strings.Split(content, "\n")
	lineMap := make(map[int]bool)
	for _, line := range changedLines {
		lineMap[line] = true
	}

	// Create a map of context lines (lines near changed lines for better pattern matching)
	contextLineMap := make(map[int]bool)
	for lineNo := range lineMap {
		contextLineMap[lineNo] = true
		// Include surrounding lines for pattern matching (decorators might be on adjacent lines)
		if lineNo > 0 {
			contextLineMap[lineNo-1] = true
		}
		if lineNo < len(lines) {
			contextLineMap[lineNo+1] = true
		}
	}

	// Check for Conformance patterns in changed/context lines
	conformancePattern := regexp.MustCompile(`\[Conformance\]|decorators\.Conformance`)

	for lineNo := range contextLineMap {
		if lineNo <= 0 || lineNo > len(lines) {
			continue
		}

		line := lines[lineNo-1] // Convert to 0-based index
		if conformancePattern.MatchString(line) {
			cd.logger.Debugf("Found Conformance pattern at line %d: %s", lineNo, strings.TrimSpace(line))
			// Make sure this line is actually changed (not just context)
			if lineMap[lineNo] {
				return true
			}
		}
	}

	return false
}
