package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// ServingOptionsVersion is the only accepted serving options contract.
const ServingOptionsVersion = 1

// ErrServingRequiresV6 rejects serving options in a catalog version that
// predates them.
var ErrServingRequiresV6 = errors.New("vLLM serving options require catalog version 6")

// ErrEmptyServingOptions rejects a present but null or empty serving options
// object. omitzero would otherwise decode it to the absent form and drop the
// key, so the accepted bytes could not round-trip.
var ErrEmptyServingOptions = errors.New("serving options object must not be empty")

// The vLLM command-line options that the KV cache and batching settings use.
// They are verified against vLLM 0.14.1, the minimum serving version.
const (
	FlagGPUMemoryUtilization = "--gpu-memory-utilization"
	FlagKVCacheDType         = "--kv-cache-dtype"
	FlagEnablePrefixCaching  = "--enable-prefix-caching"
	FlagNoPrefixCaching      = "--no-enable-prefix-caching"
	FlagPrefixCachingHash    = "--prefix-caching-hash-algo"
	FlagEnableChunkedPrefill = "--enable-chunked-prefill"
	FlagMaxNumSeqs           = "--max-num-seqs"
	FlagMaxNumBatchedTokens  = "--max-num-batched-tokens"
	FlagEnableSleepMode      = "--enable-sleep-mode"
)

// Bounds and defaults of the serving options. GPU memory utilization is a
// whole percentage, so the rendered value is exact and a vLLM update cannot
// change it.
const (
	DefaultGPUMemoryPercent = 90
	MinGPUMemoryPercent     = 50
	MaxGPUMemoryPercent     = 95
	MaxServingNumSeqs       = 64
	MinBatchedTokens        = 256
	MaxBatchedTokens        = 65536
	// KVCacheDTypeAuto stores the KV cache in the model data type.
	KVCacheDTypeAuto = "auto"
	// KVCacheDTypeFP8 needs a qualification run for the model and GPU (#453).
	// No qualification evidence exists yet, so it is refused.
	KVCacheDTypeFP8 = "fp8"
	// PrefixCachingHashSHA256 is the cryptographic prefix hash. It prevents
	// hash collisions between prefixes.
	PrefixCachingHashSHA256 = "sha256"
)

// ServingOptions are the bounded vLLM KV cache and batching settings of one
// owned launch. Absent keeps the exact legacy unit bytes and fingerprints.
// Present renders every default explicitly, so a vLLM update cannot change
// it silently. The type is a comparable value struct, like ResourceLimits.
// A zero field selects the default; MaxNumSeqs and MaxNumBatchedTokens
// render only when set, and vLLM then uses its own value.
type ServingOptions struct {
	Version int `json:"version"`
	// GPUMemoryPercent is --gpu-memory-utilization in percent: 50 to 95.
	GPUMemoryPercent uint32 `json:"gpuMemoryPercent,omitempty"`
	// KVCacheDType is "auto". "fp8" is refused until it is qualified.
	KVCacheDType string `json:"kvCacheDType,omitempty"`
	// MaxNumSeqs is 1 to 64.
	MaxNumSeqs uint32 `json:"maxNumSeqs,omitempty"`
	// MaxNumBatchedTokens is the chunked prefill budget: 256 to 65536.
	MaxNumBatchedTokens uint32 `json:"maxNumBatchedTokens,omitempty"`
	// DisablePrefixCaching renders --no-enable-prefix-caching. It is for
	// diagnosis only.
	DisablePrefixCaching bool `json:"disablePrefixCaching,omitempty"`
	// SleepMode renders --enable-sleep-mode so the supervisor can hibernate
	// vLLM weights to host RAM and discard KV cache instead of stopping the unit.
	SleepMode bool `json:"sleepMode,omitempty"`
}

// UnmarshalJSON decodes the serving options strictly in every decoder and
// rejects the empty forms with ErrEmptyServingOptions.
func (s *ServingOptions) UnmarshalJSON(raw []byte) error {
	type plain ServingOptions
	var decoded plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&decoded); err != nil {
		return err
	}
	if ServingOptions(decoded).IsZero() {
		return ErrEmptyServingOptions
	}
	*s = ServingOptions(decoded)
	return nil
}

// IsZero reports absent serving options.
func (s ServingOptions) IsZero() bool {
	return s == ServingOptions{}
}

// Validate applies the bounded schema.
func (s ServingOptions) Validate() error {
	if s.Version != ServingOptionsVersion {
		return errors.New("unsupported serving options version")
	}
	if s.GPUMemoryPercent != 0 && !ValidGPUMemoryPercent(s.GPUMemoryPercent) {
		return fmt.Errorf("GPU memory utilization must be %d to %d percent", MinGPUMemoryPercent, MaxGPUMemoryPercent)
	}
	if err := ValidKVCacheDType(s.KVCacheDType); err != nil {
		return err
	}
	if s.MaxNumSeqs > MaxServingNumSeqs {
		return fmt.Errorf("maximum concurrent sequences must be 1 to %d", MaxServingNumSeqs)
	}
	if s.MaxNumBatchedTokens != 0 && (s.MaxNumBatchedTokens < MinBatchedTokens || s.MaxNumBatchedTokens > MaxBatchedTokens) {
		return fmt.Errorf("maximum batched tokens must be %d to %d", MinBatchedTokens, MaxBatchedTokens)
	}
	return nil
}

// ValidKVCacheDType accepts an empty value, which selects "auto", and
// "auto". fp8 needs a qualification run for the model and GPU, which does
// not exist yet (#453).
func ValidKVCacheDType(value string) error {
	switch value {
	case "", KVCacheDTypeAuto:
		return nil
	case KVCacheDTypeFP8:
		return errors.New("KV cache dtype fp8 needs a qualification run for this model and GPU (#453); use auto")
	}
	return errors.New("KV cache dtype must be auto")
}

// ValidGPUMemoryPercent reports whether a GPU memory utilization percentage
// is in bounds. The adopted grammar shares it.
func ValidGPUMemoryPercent(percent uint32) bool {
	return percent >= MinGPUMemoryPercent && percent <= MaxGPUMemoryPercent
}

// EffectiveGPUMemoryPercent is the rendered GPU memory utilization.
func (s ServingOptions) EffectiveGPUMemoryPercent() uint32 {
	if s.GPUMemoryPercent == 0 {
		return DefaultGPUMemoryPercent
	}
	return s.GPUMemoryPercent
}

// FormatGPUMemoryUtilization renders a percentage as the exact two-digit
// fraction that vLLM reads, such as 0.90.
func FormatGPUMemoryUtilization(percent uint32) string {
	return fmt.Sprintf("0.%02d", percent)
}

// Args returns the exact rendered vLLM arguments in their fixed order.
// Absent options render nothing.
func (s ServingOptions) Args() []string {
	if s.IsZero() {
		return nil
	}
	args := []string{
		FlagGPUMemoryUtilization, FormatGPUMemoryUtilization(s.EffectiveGPUMemoryPercent()),
		FlagKVCacheDType, KVCacheDTypeAuto,
		FlagEnableChunkedPrefill,
	}
	if s.DisablePrefixCaching {
		args = append(args, FlagNoPrefixCaching)
	} else {
		args = append(args, FlagEnablePrefixCaching, FlagPrefixCachingHash, PrefixCachingHashSHA256)
	}
	if s.MaxNumSeqs != 0 {
		args = append(args, FlagMaxNumSeqs, strconv.FormatUint(uint64(s.MaxNumSeqs), 10))
	}
	if s.MaxNumBatchedTokens != 0 {
		args = append(args, FlagMaxNumBatchedTokens, strconv.FormatUint(uint64(s.MaxNumBatchedTokens), 10))
	}
	if s.SleepMode {
		args = append(args, FlagEnableSleepMode)
	}
	return args
}

// GPUReservationMiB is the memory vLLM reserves at start: utilization times
// total VRAM, rounded up, independent of the model size.
func GPUReservationMiB(percent uint32, totalMiB uint64) uint64 {
	if totalMiB > (^uint64(0)-99)/100 {
		return totalMiB
	}
	return (totalMiB*uint64(percent) + 99) / 100
}

// HasServingOptions reports whether the profile's owned launch carries
// serving options. It is the one test for the version rule. The setup client
// mirrors it in clients/setup/review.mjs.
func (p WorkloadProfile) HasServingOptions() bool {
	return p.NativeModel != nil && p.NativeModel.Owned != nil && !p.NativeModel.Owned.Serving.IsZero()
}

// HasServingOptions reports whether any owned launch carries serving options.
func (c Catalog) HasServingOptions() bool {
	return slices.ContainsFunc(c.Profiles, WorkloadProfile.HasServingOptions)
}
