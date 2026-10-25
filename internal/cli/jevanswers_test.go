package cli

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/campaign"
)

func TestAnswerRecordIsNotAttachedToAStubCampaign(t *testing.T) {
	var out bytes.Buffer
	engine := &campaign.Engine{}
	closeRecord, err := attachAnswerRecord(&out, engine, optimizeFlags{runADKStub: true, jevAnswers: filepath.Join(t.TempDir(), "a.db")})
	require.NoError(t, err)
	require.NoError(t, closeRecord())
	require.Empty(t, out.String(), "a stub campaign says nothing about a record it did not open")
	require.Empty(t, engine.State().JevAnswersPath, "a stub's answers must never enter the record a live campaign reads")
}

func TestAnswerRecordIsOffWhenTheOperatorSaysSo(t *testing.T) {
	for _, flag := range []string{"off", "none"} {
		var out bytes.Buffer
		engine := &campaign.Engine{}
		closeRecord, err := attachAnswerRecord(&out, engine, optimizeFlags{runADK: true, jevAnswers: flag})
		require.NoError(t, err)
		require.NoError(t, closeRecord())
		require.Empty(t, out.String())
		require.Empty(t, engine.State().JevAnswersPath)
	}
}

func TestAnswerRecordIsAttachedFromTheOperatorPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "answers.db")
	var out bytes.Buffer
	engine := &campaign.Engine{}
	closeRecord, err := attachAnswerRecord(&out, engine, optimizeFlags{runADK: true, jevAnswers: path})
	require.NoError(t, err)
	require.Equal(t, path, engine.State().JevAnswersPath)
	require.Contains(t, out.String(), "jev answers: reusing and extending the record at "+path)
	require.FileExists(t, path)
	require.NoError(t, closeRecord())
}

func TestAnswerRecordPathDefaultsBesideTheCampaigns(t *testing.T) {
	path, err := answerRecordPath("")
	require.NoError(t, err)
	require.Contains(t, path, filepath.Join("gotorque", "jev-answers.db"))
	defaultPath, err := campaign.AnswerRecordPath()
	require.NoError(t, err)
	require.Equal(t, defaultPath, path)
}

func TestAnUnopenableAnswerRecordWarnsAndDoesNotStopTheCampaign(t *testing.T) {
	// A directory where the database should be: bbolt cannot open it.
	path := t.TempDir()
	var out bytes.Buffer
	engine := &campaign.Engine{}
	closeRecord, err := attachAnswerRecord(&out, engine, optimizeFlags{runADK: true, jevAnswers: path})
	require.NoError(t, err, "a record is a reproducibility aid, never a reason to fail a campaign")
	require.NoError(t, closeRecord())
	require.Contains(t, out.String(), "jev answers: recording disabled for this campaign")
	require.Empty(t, engine.State().JevAnswersPath)
}
