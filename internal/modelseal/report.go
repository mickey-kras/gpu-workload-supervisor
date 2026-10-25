package modelseal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// ReportVersion is the only scan report format.
const ReportVersion = 1

// Report bounds. The verdict limits follow the pinned scanner defaults with
// a margin: it evaluates ten candidates with short token sequences.
const (
	MaxReportBytes     = 1 << 20
	maxVerdictBytes    = 1 << 20
	maxCandidates      = 16
	maxEvaluated       = 1024
	maxCandidateBytes  = 4096
	maxCandidateTokens = 512
)

// Check outcomes in a report.
const (
	OutcomePassed      = "passed"
	OutcomeFindings    = "findings"
	OutcomeUnsupported = "unsupported"
	OutcomeSkipped     = "skipped"
	OutcomeError       = "error"
	OutcomeTimeout     = "timeout"
)

// Check is one named step of an assessment and its outcome.
type Check struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

// Candidate is one recovered trigger candidate above the threshold. The
// string is model output: keep the report protected.
type Candidate struct {
	String      string  `json:"string"`
	TokenIDs    []int   `json:"token_ids"`
	MetricValue float64 `json:"metric_value"`
	ASR         float64 `json:"asr"`
}

// ScannerVerdict is the verdict file of the pinned scanner revision.
type ScannerVerdict struct {
	Verdict                     string      `json:"verdict"`
	ExperimentType              string      `json:"experiment_type"`
	MetricName                  string      `json:"metric_name"`
	MetricValue                 float64     `json:"metric_value"`
	Threshold                   float64     `json:"threshold"`
	CandidatesAboveThreshold    []Candidate `json:"candidates_above_threshold"`
	NumCandidatesAboveThreshold int         `json:"num_candidates_above_threshold"`
	TotalCandidatesEvaluated    int         `json:"total_candidates_evaluated"`
	BaselineASR                 *float64    `json:"baseline_asr,omitempty"`
}

// Report is the protected detail behind scan.reportSHA256. It binds the
// artifact digest, so a report never moves to other bytes. It has no
// timestamp, so the same inputs give the same bytes.
type Report struct {
	Version  int                    `json:"version"`
	Artifact string                 `json:"artifact"`
	Scanner  attestation.Scanner    `json:"scanner"`
	Status   attestation.ScanStatus `json:"status"`
	Coverage attestation.Coverage   `json:"coverage,omitempty"`
	Checks   []Check                `json:"checks"`
	Result   *ScannerVerdict        `json:"result,omitempty"`
}

var metricNames = map[string]string{"exp1": "aggregate_similarity", "exp2": "abs_relative_asr_delta"}

// readVerdict reads the verdict file that the staging wrapper wrote.
func readVerdict(scratch string) (ScannerVerdict, error) {
	f, err := openRegular(filepath.Join(scratch, "verdict.json"))
	if err != nil {
		return ScannerVerdict{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxVerdictBytes+1))
	var verdict ScannerVerdict
	if err != nil || len(raw) > maxVerdictBytes || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), maxVerdictBytes, &verdict) != nil {
		return ScannerVerdict{}, errors.New("scanner verdict is not bounded strict JSON")
	}
	return verdict, nil
}

// consistent checks the verdict against the configuration and itself. A
// verdict without evaluated candidates is inconclusive, never a pass.
func (v ScannerVerdict) consistent(config ScannerConfig) bool {
	suspicious := v.MetricValue >= v.Threshold
	return v.ExperimentType == config.Experiment &&
		v.MetricName == metricNames[config.Experiment] &&
		v.Threshold == config.Threshold &&
		((v.Verdict == "SUSPICIOUS" && suspicious) || (v.Verdict == "NOT_SUSPICIOUS" && !suspicious)) &&
		(v.BaselineASR != nil) == (config.Experiment == "exp2") &&
		v.TotalCandidatesEvaluated >= 1 && v.TotalCandidatesEvaluated <= maxEvaluated &&
		v.NumCandidatesAboveThreshold == len(v.CandidatesAboveThreshold) &&
		len(v.CandidatesAboveThreshold) <= min(maxCandidates, v.TotalCandidatesEvaluated) &&
		(v.Verdict == "SUSPICIOUS") == (len(v.CandidatesAboveThreshold) > 0) &&
		boundedCandidates(v.CandidatesAboveThreshold)
}

func boundedCandidates(candidates []Candidate) bool {
	for _, candidate := range candidates {
		if len(candidate.String) > maxCandidateBytes || len(candidate.TokenIDs) == 0 || len(candidate.TokenIDs) > maxCandidateTokens {
			return false
		}
	}
	return true
}

// marshalReport encodes a report and refuses one above MaxReportBytes.
func marshalReport(report Report) ([]byte, error) {
	raw, err := json.Marshal(report)
	if err != nil || len(raw) > MaxReportBytes {
		return nil, errors.New("scan report exceeds its limit")
	}
	return raw, nil
}
