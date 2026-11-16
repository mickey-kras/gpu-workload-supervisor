package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// ErrServingOptionUnsupported refuses a serving option or value that the
// installed vLLM does not list in its help. Setup never passes an unknown
// option through.
var ErrServingOptionUnsupported = errors.New("the installed vLLM does not support a serving option")

// servingHelpTimeout bounds one vLLM help run. vLLM imports its engine
// modules before it prints help, which takes seconds.
const servingHelpTimeout = 2 * time.Minute

// servingHelpArgs list every vLLM serve option. A plain --help lists only
// the option groups since vLLM 0.10; --help=all lists every option.
var servingHelpArgs = []string{"serve", "--help=all"}

// planCheckedOwnedUnits plans the owned unit changes like planOwnedUnits.
// Planning qualifies the owned executables; their help then proves each
// serving option before any file changes.
func (b Backend) planCheckedOwnedUnits(ctx context.Context, req Request, accepted control.CatalogSnapshot, home string) (unitPlan, error) {
	plan, err := b.planOwnedUnits(req, accepted, home)
	if err != nil {
		return plan, err
	}
	return plan, b.checkServingOptions(ctx, req.Catalog)
}

// checkServingOptions checks each rendered serving option of every owned
// launch against the help of the executable that the unit runs. It runs
// after the owned qualification proved the executable trusted. The command
// runner bounds the output; a cut or failed run refuses the options.
func (b Backend) checkServingOptions(ctx context.Context, catalog control.Catalog) error {
	helps := map[string]servingHelp{}
	for _, p := range catalog.Profiles {
		if !p.HasServingOptions() {
			continue
		}
		executable := gpuruntime.OwnedExecutable(*p.NativeModel)
		help, ok := helps[executable]
		if !ok {
			var err error
			help, err = b.readServingHelp(ctx, executable)
			if err != nil {
				return fmt.Errorf("%w: %s: %w", ErrServingOptionUnsupported, p.ID, err)
			}
			helps[executable] = help
		}
		if err := help.check(p.NativeModel.Owned.Serving.Args()); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrServingOptionUnsupported, p.ID, err)
		}
	}
	return nil
}

func (b Backend) readServingHelp(ctx context.Context, executable string) (servingHelp, error) {
	if b.runCommand == nil {
		return nil, errors.New("no command runner to read the vLLM help")
	}
	ctx, cancel := context.WithTimeout(ctx, servingHelpTimeout)
	defer cancel()
	output, err := b.runCommand(ctx, executable, servingHelpArgs...)
	if err != nil {
		return nil, fmt.Errorf("vLLM help could not be read: %w", err)
	}
	if len(output) >= commandOutputLimit {
		return nil, errors.New("vLLM help exceeds 1 MiB")
	}
	help := parseServingHelp(output)
	if len(help) == 0 {
		return nil, errors.New("vLLM help lists no options")
	}
	return help, nil
}

// maxHelpEntryIndent is the deepest indentation of an argparse entry line.
const maxHelpEntryIndent = 6

// servingHelp maps each listed option to its listed choices. An option
// without a choice list maps to nil.
type servingHelp map[string][]string

// parseServingHelp reads the option entries of argparse help. An entry line
// starts with indentation and an option, such as
// "  --kv-cache-dtype {auto,fp8}  text" or
// "  --enable-prefix-caching, --no-enable-prefix-caching". Text in option
// descriptions and the usage line are not entries.
func parseServingHelp(output []byte) servingHelp {
	help := servingHelp{}
	for line := range bytes.Lines(output) {
		entry := strings.TrimRight(string(line), "\r\n")
		trimmed := strings.TrimLeft(entry, " ")
		// argparse indents entries by two spaces for each section level and
		// wrapped descriptions by 24, so a description line is not an entry.
		indent := len(entry) - len(trimmed)
		if indent == 0 || indent > maxHelpEntryIndent || !strings.HasPrefix(trimmed, "--") {
			continue
		}
		// Two spaces separate the option column from its description.
		options, _, _ := strings.Cut(trimmed, "  ")
		for option := range strings.SplitSeq(options, ", ") {
			name, rest, _ := strings.Cut(option, " ")
			if !strings.HasPrefix(name, "--") {
				continue
			}
			help[name] = helpChoices(rest)
		}
	}
	return help
}

// helpChoices reads an argparse choice list such as {auto,fp8}.
func helpChoices(text string) []string {
	if !strings.HasPrefix(text, "{") || !strings.HasSuffix(text, "}") {
		return nil
	}
	return strings.Split(strings.TrimSuffix(strings.TrimPrefix(text, "{"), "}"), ",")
}

// check refuses an option that the help does not list, and a value that is
// not in the listed choices of its option.
func (h servingHelp) check(args []string) error {
	for i := 0; i < len(args); i++ {
		option := args[i]
		choices, ok := h[option]
		if !ok {
			return fmt.Errorf("option %s is not available", option)
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			continue
		}
		i++
		if choices != nil && !slices.Contains(choices, args[i]) {
			return fmt.Errorf("option %s does not accept %s", option, args[i])
		}
	}
	return nil
}
