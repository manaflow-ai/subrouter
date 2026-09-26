package cutovercanary

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func writePrivateJSON(path string, value any) error {
	if err := validateOutputPath(path); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var outputEnvelope struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(b, &outputEnvelope); err != nil || outputEnvelope.Schema == "" {
		return errors.New("canary artifact schema is unavailable")
	}
	var existingInfo os.FileInfo
	if info, err := os.Lstat(path); err == nil {
		existingInfo = info
		if err := requireRecognizedArtifact(path, outputEnvelope.Schema); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".canary-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if existingInfo == nil {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("canary artifact destination changed before publication")
		}
	} else {
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(existingInfo, current) {
			return errors.New("canary artifact destination changed before publication")
		}
		if err := requireRecognizedArtifact(path, outputEnvelope.Schema); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func createPrivateJSON(path string, value any) error {
	if err := validateOutputPath(path); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".canary-exclusive-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Link publishes the fully-written inode and fails if another coordinator
	// already owns the destination. Unlike a rename, it cannot overwrite.
	if err := os.Link(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func removeIfExists(path string, expectedSchemas ...string) error {
	existing, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect canary artifact: %w", err)
	}
	if err := requireRecognizedArtifact(path, expectedSchemas...); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(existing, current) {
		return errors.New("canary artifact changed before removal")
	}
	if err := requireRecognizedArtifact(path, expectedSchemas...); err != nil {
		return err
	}
	err = os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("remove canary artifact: %w", err)
}

func requireRecognizedArtifact(path string, expectedSchemas ...string) error {
	b, err := readPrivateFile(path, 1<<20)
	if err != nil {
		return errors.New("existing file is not a recognized canary artifact")
	}
	if err := rejectDuplicateJSONKeys(b); err != nil {
		return errors.New("existing file is not a recognized canary artifact")
	}
	var envelope struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return errors.New("existing file is not a recognized canary artifact")
	}
	if len(expectedSchemas) > 0 {
		matched := false
		for _, schema := range expectedSchemas {
			matched = matched || envelope.Schema == schema
		}
		if !matched {
			return errors.New("existing file is not an owned canary artifact")
		}
	}
	var artifact any
	switch envelope.Schema {
	case ProofSchema:
		artifact = &proof{}
	case LiveStateSchema:
		artifact = &liveState{}
	case JournalSchema:
		artifact = &cleanupJournal{}
	case ChallengeSchema:
		artifact = &Challenge{}
	case WitnessSchema:
		artifact = &Witness{}
	default:
		return errors.New("existing file is not a recognized canary artifact")
	}
	if err := readStrictPrivateJSON(path, artifact); err != nil {
		return errors.New("existing file is not a recognized canary artifact")
	}
	return nil
}
