package docsync

import (
	"context"
	"errors"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules"
	"github.com/gravadigital/jiku-go/modules/docs"
	"github.com/nats-io/nats.go"
)

// RemoteDoc is a document as the engine needs it.
type RemoteDoc struct {
	ID       string
	Path     string
	FolderID string
	Version  int
	Checksum string
	// Deleted is set by Get for a deleted document, which the module still answers by id.
	Deleted bool
}

// RemoteFolder is a folder with the permission the person has on it — which only a module
// older than contract 1.3.0 means, and which is empty for a folder learned from a document.
type RemoteFolder struct {
	ID         string
	Path       string
	Permission string // read, write or manage
}

// Remote is the docs module as the engine sees it. The real one wraps a *docs.Client; tests use
// a fake, so the engine runs without NATS.
type Remote interface {
	// ListDocuments returns every readable document under pathPrefix ("" for all).
	ListDocuments(ctx context.Context, pathPrefix string) ([]RemoteDoc, error)
	// Folders returns every readable folder under pathPrefix, with the effective permission.
	// A module with contract 1.3.0 refuses it to a person (permission_denied), and lists to
	// admins and services only; the engine then learns folders from the documents' paths.
	Folders(ctx context.Context, pathPrefix string) ([]RemoteFolder, error)
	// Get returns a document's current metadata and content. A deleted document comes back too,
	// with Deleted set, as the module answers it.
	Get(ctx context.Context, id string) (RemoteDoc, []byte, error)
	// Update saves content over baseVersion and returns the new version.
	Update(ctx context.Context, id string, baseVersion int, content []byte) (int, error)
	// Create creates a document with a client id.
	Create(ctx context.Context, id, folderID, fileName string, content []byte) (RemoteDoc, error)
	// Changes reads one page of the change feed.
	Changes(ctx context.Context, sinceSeq int64) (*docs.ChangesPage, error)
	// Notifications subscribes to the person's notifications.
	Notifications(ctx context.Context) (<-chan docs.Notification, error)
}

// Registry is the module registry as the engine sees it.
type Registry interface {
	// Status is the module's state now.
	Status(ctx context.Context) (modules.State, error)
	// Watch observes the module's key: the present first, then every change.
	Watch(ctx context.Context) (<-chan modules.Change, error)
}

// NewRegistry adapts a *modules.Registry for the docs module.
func NewRegistry(r *modules.Registry) Registry { return registry{r} }

type registry struct{ r *modules.Registry }

func (r registry) Status(ctx context.Context) (modules.State, error) {
	return r.r.Status(ctx, docs.Module, docs.Protocol)
}

func (r registry) Watch(ctx context.Context) (<-chan modules.Change, error) {
	return r.r.Watch(ctx, docs.Module)
}

// Connector builds a Remote for a module record: the record says which plane to talk to, and it
// can change while the daemon runs.
type Connector func(rec modules.Record) Remote

// ClientConnector returns a Connector over a jiku.Client. Writes are recorded as the person's
// (`via: human`): the daemon uploads what somebody saved in an editor.
func ClientConnector(c *jiku.Client) Connector {
	return func(rec modules.Record) Remote {
		return NewRemote(docs.NewClient(c.On(rec.Plane()), docs.Options{Via: docs.ViaHuman, Notify: c}))
	}
}

// NewRemote adapts a *docs.Client.
func NewRemote(d *docs.Client) Remote { return remote{d} }

type remote struct{ d *docs.Client }

// syncFields are the only fields reconciliation needs. Asking for no more keeps a listing of
// thousands of documents small.
var syncFields = []string{"id", "path", "version", "checksum", "folderId"}

func (r remote) ListDocuments(ctx context.Context, pathPrefix string) ([]RemoteDoc, error) {
	q := jiku.List{Fields: syncFields, Limit: 200}
	if pathPrefix != "" {
		q.Filter = jiku.F{"pathPrefix": pathPrefix}
	}
	var out []RemoteDoc
	it := r.d.Iterate(ctx, "documents", q)
	for it.Next() {
		var d docs.Document
		if err := it.Item().Into(&d); err != nil {
			return nil, err
		}
		out = append(out, RemoteDoc{ID: d.ID, Path: d.Path, FolderID: d.FolderID, Version: d.Version, Checksum: d.Checksum})
	}
	return out, it.Err()
}

func (r remote) Folders(ctx context.Context, pathPrefix string) ([]RemoteFolder, error) {
	q := jiku.List{Include: []string{"effectivePermission"}, Limit: 200}
	if pathPrefix != "" {
		q.Filter = jiku.F{"pathPrefix": pathPrefix}
	}
	var out []RemoteFolder
	it := r.d.Iterate(ctx, "folders", q)
	for it.Next() {
		var f docs.Folder
		if err := it.Item().Into(&f); err != nil {
			return nil, err
		}
		out = append(out, RemoteFolder{ID: f.ID, Path: f.Path, Permission: f.EffectivePermission})
	}
	return out, it.Err()
}

func (r remote) Get(ctx context.Context, id string) (RemoteDoc, []byte, error) {
	d, content, err := r.d.GetContent(ctx, id, docs.GetOptions{})
	if err != nil {
		return RemoteDoc{}, nil, err
	}
	return RemoteDoc{ID: d.ID, Path: d.Path, FolderID: d.FolderID, Version: d.Version, Checksum: d.Checksum,
		Deleted: d.DeletedAt != nil}, content, nil
}

func (r remote) Update(ctx context.Context, id string, baseVersion int, content []byte) (int, error) {
	res, err := r.d.Put(ctx, id, content, docs.PutOptions{BaseVersion: baseVersion})
	if err != nil {
		return 0, err
	}
	return res.Version, nil
}

func (r remote) Create(ctx context.Context, id, folderID, fileName string, content []byte) (RemoteDoc, error) {
	res, err := r.d.New(ctx, docs.NewDocument{ID: id, FolderID: folderID, FileName: fileName, Content: content})
	if err != nil {
		return RemoteDoc{}, err
	}
	return RemoteDoc{ID: res.ID, Path: res.Path, FolderID: folderID, Version: res.Version, Checksum: docs.Checksum(content)}, nil
}

func (r remote) Changes(ctx context.Context, sinceSeq int64) (*docs.ChangesPage, error) {
	return r.d.Changes(ctx, sinceSeq, 500, "")
}

func (r remote) Notifications(ctx context.Context) (<-chan docs.Notification, error) {
	return r.d.Notifications(ctx)
}

// unreachable reports whether err means the module (or the bus) is not answering — the cue to go
// offline — rather than a refusal of this particular request.
func unreachable(err error) bool {
	return errors.Is(err, jiku.ErrNoEndpoint) || errors.Is(err, jiku.ErrTimeout) ||
		errors.Is(err, jiku.ErrNotConnected) || errors.Is(err, modules.ErrUnavailable) ||
		errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrNoServers)
}
