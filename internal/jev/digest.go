package jev

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Digest identifies a request by its exact state, question set, and model:
// the same three things Evaluate sends over the wire. TypeSafe's docs and this
// package's own baselines (baseline.go, fixkinds.go) agree that an answer is
// independent of the other questions asked alongside it, but Digest still
// includes the whole question set, not just its ids, because the criteria
// text is part of what was asked; changing it without changing the id would
// otherwise return a stale cached answer for an edited question.
//
// A cache built from Digest is only ever a memo of what a request already
// answered at this exact base revision; it is not a substitute for the
// baselines, which stay valid at any question-set size (see combinedQuestions
// in campaign/causes.go).
func Digest(req Request) (string, error) {
	payload, err := json.Marshal(struct {
		Model     string              `json:"model"`
		State     any                 `json:"state"`
		Questions map[string]Question `json:"questions"`
	}{req.Model, req.State, req.Questions})
	if err != nil {
		return "", fmt.Errorf("digest Jev request: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// digestPayload hashes any static question-set-and-state payload the same
// way Digest hashes a live request: causes.go's digest, review.go's
// reviewDigest, and canary.go's checkCanaryDigest all pin a baseline or
// recorded values to the exact text they were measured with, and all three
// should go stale the same way when that text changes.
func digestPayload(payload any) string {
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err) // static strings and questions always marshal
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
