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

package server

import (
	"net/http"

	"kubevirt.io/project-infra/external-plugins/label-detector/plugin/handler"
	"sigs.k8s.io/prow/pkg/github"
)

// GitHubEventsServer handles GitHub webhook events with HMAC verification
type GitHubEventsServer struct {
	tokenGenerator func() []byte
	eventsHandler  *handler.GitHubEventsHandler
}

// NewGitHubEventsServer creates a new GitHubEventsServer
func NewGitHubEventsServer(tokenGenerator func() []byte, eventsHandler *handler.GitHubEventsHandler) *GitHubEventsServer {
	return &GitHubEventsServer{
		tokenGenerator: tokenGenerator,
		eventsHandler:  eventsHandler,
	}
}

// ServeHTTP handles incoming HTTP requests
func (s *GitHubEventsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	eventType, eventGUID, eventPayload, eventOk, _ := github.ValidateWebhook(w, r, s.tokenGenerator)

	if !eventOk {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	event := &handler.GitHubEvent{
		Type:    eventType,
		GUID:    eventGUID,
		Payload: eventPayload,
	}
	go s.eventsHandler.Handle(event)
	w.Write([]byte("Event received. Have a nice day."))
}
