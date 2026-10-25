package docsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravadigital/jiku-go/modules/docs"
)

// Report is what `jiku docs sync status` shows. It is read from the state and the disk only; no
// connection is made, so it works while the daemon runs and while the module is down.
type Report struct {
	Root        string     `json:"root"`
	Identity    string     `json:"identity,omitempty"`
	ServiceName string     `json:"serviceName,omitempty"`
	LastSeq     int64      `json:"lastSeq"`
	LastFull    *time.Time `json:"lastFull,omitempty"`
	// LastChange is the last change applied, and LastChangeUp whether it was an upload.
	LastChange   *time.Time `json:"lastChange,omitempty"`
	LastChangeUp bool       `json:"lastChangeUploaded,omitempty"`
	// StateFile is the state database read.
	StateFile string `json:"stateFile"`
	// Service is the systemd user service's state, filled in by the command when the user
	// manager can be asked.
	Service   string `json:"service,omitempty"`
	Documents int    `json:"documents"`
	ReadOnly  int    `json:"readOnly"`
	// Pending are module paths whose local content has not been uploaded: edits, and new
	// files.
	Pending   []string   `json:"pending"`
	Conflicts []Conflict `json:"conflicts"`
	Recent    []LogEntry `json:"recent"`
}

// Conflict is a document waiting for the person to merge a remote copy.
type Conflict struct {
	Path string `json:"path"`
	Copy string `json:"copy"`
}

// ErrNotSynced is an instance with no state, or a state for another folder: the daemon never
// synced this folder for this instance.
var ErrNotSynced = errors.New("docsync: this folder has never been synced")

// Status builds the report from the state in stateDir. root is the folder asked about, "" for
// whichever the state describes; a state for another folder is ErrNotSynced.
func Status(stateDir, root string, recent int) (*Report, error) {
	dbPath := StatePath(stateDir)
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: there is no %s. Start it with `jiku docs sync`, or "+
			"`systemctl --user start jiku-docs-sync`", ErrNotSynced, dbPath)
	}
	st, err := OpenState(dbPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	have, err := st.Meta(MetaRoot)
	if err != nil {
		return nil, err
	}
	switch {
	case root == "" && have == "":
		return nil, fmt.Errorf("%w: %s does not say which folder it is for; run the sync once", ErrNotSynced, dbPath)
	case root == "":
		root = have
	case have != "" && have != root:
		return nil, fmt.Errorf("%w: %s is synced for this instance, not %s", ErrNotSynced, have, root)
	}

	rep := &Report{Root: root, StateFile: dbPath, Pending: []string{}, Conflicts: []Conflict{}}
	if t, up, err := st.LastChange(); err == nil && !t.IsZero() {
		rep.LastChange, rep.LastChangeUp = &t, up
	}
	rep.Identity, _ = st.Meta(MetaIdentity)
	rep.ServiceName, _ = st.Meta(MetaServiceName)
	if rep.LastSeq, err = st.LastSeq(); err != nil {
		return nil, err
	}
	if t, err := st.LastFull(); err == nil && !t.IsZero() {
		rep.LastFull = &t
	}
	docsList, err := st.Docs()
	if err != nil {
		return nil, err
	}
	rep.Documents = len(docsList)
	for _, d := range docsList {
		if d.ReadOnly {
			rep.ReadOnly++
		}
		if d.Conflict != "" {
			rep.Conflicts = append(rep.Conflicts, Conflict{Path: d.Path, Copy: d.Conflict})
		}
	}

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && ignoredName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rp, ok := remotePath(root, p)
		if !ok || hiddenPath(rp) || !strings.EqualFold(path.Ext(rp), ".md") {
			return nil
		}
		rec, err := st.DocByPath(rp)
		if err != nil {
			return err
		}
		if rec != nil && rec.Conflict != "" {
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if rec == nil || docs.Checksum(content) != rec.Checksum {
			rep.Pending = append(rep.Pending, rp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if rep.Recent, err = st.RecentLog(recent); err != nil {
		return nil, err
	}
	if rep.Recent == nil {
		rep.Recent = []LogEntry{}
	}
	return rep, nil
}
