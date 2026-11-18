package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/arkade-os/executor/pkg/template"
	"github.com/stretchr/testify/require"
)

func TestNewTemplateDerivesFieldsFromDocument(t *testing.T) {
	doc := testTemplate(t, "renew.json")
	tpl, err := newTemplate(doc, testArtifacts(t))
	require.NoError(t, err)
	require.Len(t, tpl.ID, 64)
	require.Equal(t, doc, tpl.Document)
	require.Equal(t, []string{testArtifactID(t)}, tpl.ArtifactIDs)
	require.Equal(t, template.Proof, tpl.Type)
	require.Equal(t, TemplateStatusActive, tpl.Status)
	require.Zero(t, tpl.Failures)
	require.True(t, tpl.CreatedAt.IsZero())
	require.Contains(t, tpl.Params, Param{Name: "owner", Type: "pubkey"})
	require.Contains(t, tpl.Params, Param{Name: "exit_delay", Type: "int"})

	var v any
	require.NoError(t, json.Unmarshal(doc, &v))
	spaced, err := json.MarshalIndent(v, "", "    ")
	require.NoError(t, err)
	again, err := newTemplate(spaced, testArtifacts(t))
	require.NoError(t, err)
	require.Equal(t, tpl.ID, again.ID)
}

func TestNewTemplateNeedsNoArtifactsWhenItReferencesNone(t *testing.T) {
	tpl, err := newTemplate(testTemplate(t, "vhtlc_claim.json"), nil)
	require.NoError(t, err)
	require.Empty(t, tpl.ArtifactIDs)
	require.NotEmpty(t, tpl.Params)
	require.NotEmpty(t, tpl.Type)
}

func TestDecodeRejectsInvalidDocument(t *testing.T) {
	for name, doc := range map[string][]byte{
		"not json": []byte("nope"), "empty": nil,
		"wrong schema": []byte(`{"format":"template/v1"}`),
	} {
		_, err := template.Decode(doc)
		require.Error(t, err, name)
	}
}

func TestNewTemplateServesAnOnchainTemplateOnlyWithAnArkadePacket(t *testing.T) {
	doc, err := os.ReadFile("../../../../pkg/template/testdata/onchain_release.json")
	require.NoError(t, err)
	_, err = newTemplate(doc, nil)
	require.ErrorIs(t, err, ErrUnsupported)
	require.NotErrorIs(t, err, ErrInvalidDocument)
	require.ErrorContains(t, err, "arkade packet")

	doc, err = os.ReadFile("../execution/testdata/onchain_arkade.json")
	require.NoError(t, err)
	tpl, err := newTemplate(doc, nil)
	require.NoError(t, err)
	require.Equal(t, template.Onchain, tpl.Type)

	free := bytes.Replace(doc, []byte(`  "fees": {"from": "funds", "max": 1000},
`), nil, 1)
	require.NotEqual(t, doc, free)
	_, err = newTemplate(free, nil)
	require.ErrorIs(t, err, ErrUnsupported)
	require.ErrorContains(t, err, "must pay their fee")
}

func TestNewTemplateRefusesAnOffchainTemplateWithoutAnArkadePacket(t *testing.T) {
	doc, err := os.ReadFile("../../../../pkg/template/testdata/delegate_release.json")
	require.NoError(t, err)
	doc = bytes.Replace(doc, []byte(`"type": "proof"`), []byte(`"type": "virtual"`), 1)
	doc = bytes.Replace(doc, []byte(`,
  "fees": {"from": "funds", "max": 1000}`), nil, 1)
	_, err = newTemplate(doc, nil)
	require.ErrorIs(t, err, ErrUnsupported)
	require.NotErrorIs(t, err, ErrInvalidDocument)
	require.ErrorContains(t, err, "arkade packet")

	_, err = newTemplate(testTemplate(t, "vhtlc_claim.json"), nil)
	require.NoError(t, err)
}

func TestNewTemplateRefusesAllOutputs(t *testing.T) {
	doc := testTemplate(t, "claim.json")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(doc, &decoded))
	trigger := decoded["delegation"].(map[string]any)["trigger"].(map[string]any)
	trigger["htlc"].(map[string]any)["all_outputs"] = true
	raw, err := json.Marshal(decoded)
	require.NoError(t, err)
	_, err = newTemplate(raw, nil)
	require.ErrorIs(t, err, ErrUnsupported)
	require.ErrorContains(t, err, "all_outputs")
}

func TestNewTemplateRejectsAReferencedArtifactThatIsNotStored(t *testing.T) {
	_, err := newTemplate(testTemplate(t, "renew.json"), nil)
	require.ErrorIs(t, err, ErrInvalidDocument)
}

func TestReenablingATemplateResetsFailuresAndUnknownStatusIsRefused(t *testing.T) {
	tpl := Template{Status: TemplateStatusActive, Failures: 3}
	require.NoError(t, tpl.SetStatus(TemplateStatusDisabled))
	require.Equal(t, TemplateStatusDisabled, tpl.Status)
	require.Equal(t, 3, tpl.Failures)

	require.NoError(t, tpl.SetStatus(TemplateStatusActive))
	require.Equal(t, TemplateStatusActive, tpl.Status)
	require.Zero(t, tpl.Failures)

	tpl.Failures = 2
	require.ErrorIs(t, tpl.SetStatus("paused"), ErrInvalidStatus)
	require.ErrorIs(t, tpl.SetStatus(""), ErrInvalidStatus)
	require.Equal(t, TemplateStatusActive, tpl.Status)
	require.Equal(t, 2, tpl.Failures)
}

func newTemplate(doc []byte, artifacts map[string][]byte) (*Template, error) {
	decoded, err := template.Decode(doc)
	if err != nil {
		return nil, err
	}
	return NewDecodedTemplate(decoded, artifacts)
}

func testTemplate(t *testing.T, name string) []byte {
	t.Helper()
	doc, err := os.ReadFile("../../../../pkg/template/testdata/" + name)
	if errors.Is(err, fs.ErrNotExist) {
		doc, err = os.ReadFile("../../../../templates/" + name)
	}
	require.NoError(t, err)
	return doc
}

func testArtifactID(t *testing.T) string {
	t.Helper()
	a, err := NewArtifact(testDocument(t), 0)
	require.NoError(t, err)
	return a.ID
}

// testArtifacts serves the one default artifact.
func testArtifacts(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{testArtifactID(t): testDocument(t)}
}
