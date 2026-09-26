package claude

import "github.com/manaflow-ai/subrouter/internal/fsutil"

func renameProfileInstanceNoReplace(source, destination string) error {
	return fsutil.RenameNoReplace(source, destination)
}
