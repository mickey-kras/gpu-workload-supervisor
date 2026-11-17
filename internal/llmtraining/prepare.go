package llmtraining

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Prepare writes a private plan without replacing existing files. The config
// inside it is candidate material, not an executable supervisor adapter recipe.
func Prepare(recipePath, planPath string) error {
	if !absolute(recipePath) || !absolute(planPath) || recipePath == planPath {
		return errors.New("candidate recipe and plan require distinct absolute paths")
	}
	if err := noLinks(recipePath); err != nil {
		return err
	}
	if err := noLinks(filepath.Dir(planPath)); err != nil {
		return err
	}
	f, err := openRegular(recipePath)
	if err != nil {
		return errors.New("candidate recipe is unreadable")
	}
	defer f.Close()
	r, err := ReadRecipe(f)
	if err != nil {
		return err
	}
	for _, path := range []string{r.OutputDirectory, r.Model.Inventory.Root, r.Tokenizer.Inventory.Root, r.Runtime.Inventory.Root, r.Train.Path, r.HeldOut.Path} {
		if within(planPath, path) {
			return errors.New("candidate plan overlaps an input or training output")
		}
	}
	ram, storage, err := available(r.OutputDirectory)
	if err != nil {
		return err
	}
	plan, err := Compile(r, ram, storage)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return errors.New("cannot encode candidate plan")
	}
	return publishPlan(planPath, append(raw, '\n'))
}

func publishPlan(planPath string, raw []byte) error {
	return publishPlanWithDirectorySync(planPath, raw, syncPlanDirectory)
}

func publishPlanWithDirectorySync(planPath string, raw []byte, syncDirectory func(string) error) (err error) {
	// Stage privately beside the destination, then publish the complete closed
	// file with a hard link: unlike Rename, Link cannot replace an existing plan.
	out, err := os.CreateTemp(filepath.Dir(planPath), ".candidate-plan-*")
	if err != nil {
		return errors.New("candidate plan cannot be created; destination must not exist")
	}
	published := false
	var staged os.FileInfo
	defer func() {
		err = cleanupPlanPublication(out.Name(), planPath, staged, published, err)
	}()
	_, writeErr := out.Write(raw)
	syncErr := out.Sync()
	info, statErr := out.Stat()
	staged = info
	closeErr := out.Close()
	if writeErr != nil || syncErr != nil || statErr != nil || closeErr != nil {
		return errors.New("candidate plan write failed")
	}
	if os.Link(out.Name(), planPath) != nil {
		return errors.New("candidate plan cannot be created; destination must not exist")
	}
	published = true
	if syncDirectory(filepath.Dir(planPath)) != nil {
		return errors.New("candidate plan directory sync failed")
	}
	return nil
}

func syncPlanDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func cleanupPlanPublication(temporary, destination string, staged os.FileInfo, published bool, result error) error {
	if os.Remove(temporary) != nil {
		result = errors.Join(result, errors.New("candidate plan temporary file cleanup failed"))
	}
	// Link never replaces an existing destination. Only a successful link from
	// this call may be rolled back when publication or cleanup reports failure.
	if result != nil && published {
		current, err := os.Lstat(destination)
		if err != nil || !os.SameFile(staged, current) {
			return errors.Join(result, errors.New("candidate plan publication destination changed during rollback"))
		}
		if os.Remove(destination) != nil {
			result = errors.Join(result, errors.New("candidate plan publication rollback failed"))
		}
	}
	return result
}

func available(output string) (uint64, uint64, error) {
	var memory unix.Sysinfo_t
	var disk unix.Statfs_t
	if unix.Sysinfo(&memory) != nil || unix.Statfs(output, &disk) != nil || disk.Bsize <= 0 || memory.Unit == 0 {
		return 0, 0, errors.New("candidate resource availability could not be read")
	}
	if memory.Freeram > math.MaxUint64/uint64(memory.Unit) || disk.Bavail > math.MaxUint64/uint64(disk.Bsize) {
		return 0, 0, errors.New("candidate resource availability overflows")
	}
	return memory.Freeram * uint64(memory.Unit), disk.Bavail * uint64(disk.Bsize), nil
}
