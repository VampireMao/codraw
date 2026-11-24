// Package roomsvc implements the REST-facing room service by combining the
// persistence store, the live hub and the auth primitives.
//
// It exists so internal/api never imports internal/store or internal/auth
// directly: the API layer depends on a narrow interface, and the wiring lives
// here where it can be tested without HTTP.
package roomsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/codraw/codraw/backend/internal/api"
	"github.com/codraw/codraw/backend/internal/auth"
	"github.com/codraw/codraw/backend/internal/config"
	"github.com/codraw/codraw/backend/internal/hub"
	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
)

// Store is the persistence surface this service needs. The memory and Postgres
// stores both implement it.
type Store interface {
	room.Store
}

// Service implements api.RoomService.
type Service struct {
	store    Store
	hub      *hub.Hub
	log      *slog.Logger
	cfg      *config.Config
	now      func() time.Time
	makeRoom func(ctx context.Context, rec *room.Record) (*room.Handle, error)
	// signer verifies readonly tokens for the REST credential check; nil means
	// readonly tokens are not configured.
	signer *auth.TokenSigner
}

// Options configures the service.
type Options struct {
	Store Store
	Hub   *hub.Hub
	// Factory builds a live room from persisted state. Injected so the same
	// service drives both memory and Postgres stores.
	Factory func(ctx context.Context, rec *room.Record) (*room.Handle, error)
	Config  *config.Config
	Logger  *slog.Logger
	Now     func() time.Time
	// Signer verifies readonly tokens for VerifyCredential. Optional.
	Signer *auth.TokenSigner
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Store == nil || opts.Hub == nil || opts.Factory == nil {
		return nil, errors.New("roomsvc: Store, Hub and Factory are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		store:    opts.Store,
		hub:      opts.Hub,
		log:      opts.Logger,
		cfg:      opts.Config,
		now:      opts.Now,
		makeRoom: opts.Factory,
		signer:   opts.Signer,
	}, nil
}

var _ api.RoomService = (*Service)(nil)

// CreateRoom generates a unique code, persists the hashes and starts the room.
func (s *Service) CreateRoom(ctx context.Context, title string) (*api.CreatedRoom, error) {
	title = protocol.TruncateTitle(title, s.cfg.Limits.MaxTitleRunes)
	keys, err := auth.NewKeys()
	if err != nil {
		return nil, err
	}

	now := s.now()
	// Retry on collision. With 31^6 ≈ 8.9e8 codes and a room-per-minute creation
	// rate, a collision is vanishingly rare; the retry exists so a collision is a
	// non-event rather than a 500.
	var code string
	for attempt := 0; attempt < 8; attempt++ {
		candidate, err := room.Generate()
		if err != nil {
			return nil, fmt.Errorf("generate room code: %w", err)
		}
		rec := &room.Record{
			Code:       candidate,
			Title:      title,
			Epoch:      1,
			CreatedAt:  now,
			ShapeCount: 0,
		}
		err = s.store.CreateRoom(ctx, rec)
		if err == nil {
			code = candidate
			break
		}
		if !errors.Is(err, room.ErrDuplicateRoom) {
			return nil, fmt.Errorf("persist room: %w", err)
		}
	}
	if code == "" {
		return nil, errors.New("could not allocate a unique room code")
	}

	// Keep the hashes with the room; the plaintext keys only live in this call.
	if err := s.store.SetKeys(ctx, code, keys.EditorHash, keys.ViewerHash); err != nil {
		// Roll the room back so a half-created room cannot be joined.
		_ = s.store.DeleteRoom(ctx, code)
		return nil, fmt.Errorf("persist keys: %w", err)
	}

	// Start the live room immediately so the creator's WebSocket finds it without
	// a second load round trip.
	rec := &room.Record{Code: code, Title: title, Epoch: 1, CreatedAt: now}
	rh, err := s.makeRoom(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("start room: %w", err)
	}
	if err := s.hub.RegisterRoom(code, rh); err != nil {
		// Already registered: a join raced the create. That is fine.
		s.log.Debug("room already registered after create", "room", code)
	}

	return &api.CreatedRoom{
		Code:      code,
		Title:     title,
		CreatedAt: now,
		EditorKey: keys.EditorKey,
		ViewerKey: keys.ViewerKey,
	}, nil
}

// GetRoom returns persisted metadata merged with live stats.
func (s *Service) GetRoom(ctx context.Context, code string) (*api.RoomMeta, error) {
	meta, err := s.store.GetRoomMeta(ctx, code)
	if err != nil {
		return nil, err
	}
	return &api.RoomMeta{
		Code:         meta.Code,
		Title:        meta.Title,
		Epoch:        meta.Epoch,
		Seq:          meta.Seq,
		ShapeCount:   meta.ShapeCount,
		CreatedAt:    meta.CreatedAt,
		LastActiveAt: meta.LastActiveAt,
		RevokedAt:    meta.RevokedAt,
	}, nil
}

// VerifyKey resolves the role a presented key would hand out.
//
// Since docs/PERMISSIONS.md §2.1 a key is an invitation credential, not an
// authorisation: it no longer grants editor. The role is always viewer, and only
// the owner can promote a member afterwards.
func (s *Service) VerifyKey(ctx context.Context, code, key string) (string, error) {
	editorHash, viewerHash, _, err := s.store.VerifyKeyHash(ctx, code)
	if err != nil {
		return "", err
	}
	role, _ := auth.RoleForKey(key, editorHash, viewerHash)
	return role, nil
}

// VerifyEditorKey reports whether key is the room's editor key. It is the only
// surviving privilege of that credential: issuing and revoking readonly tokens
// (docs/PROTOCOL.md §1.3, §1.4).
func (s *Service) VerifyEditorKey(ctx context.Context, code, key string) (bool, error) {
	editorHash, _, _, err := s.store.VerifyKeyHash(ctx, code)
	if err != nil {
		return false, err
	}
	return auth.IsEditorKey(key, editorHash), nil
}

// VerifyCredential reports whether a room key or a readonly token is a valid
// credential for the room. The paged snapshot endpoint exposes the drawing, so it
// accepts either (docs/PROTOCOL.md §1.5). A false result is a policy answer, not
// an error; only a store failure is returned as err.
func (s *Service) VerifyCredential(ctx context.Context, code, key, rt string) (bool, error) {
	editorHash, viewerHash, revokedAt, err := s.store.VerifyKeyHash(ctx, code)
	if err != nil {
		return false, err
	}
	if rt != "" {
		if s.signer == nil {
			return false, nil
		}
		if _, verr := s.signer.Verify(rt, code, revokedAt, s.now()); verr != nil {
			return false, nil
		}
		return true, nil
	}
	if key == "" {
		return false, nil
	}
	_, ok := auth.RoleForKey(key, editorHash, viewerHash)
	return ok, nil
}

// BumpReadonlyRevocation invalidates every token issued before now.
func (s *Service) BumpReadonlyRevocation(ctx context.Context, code string) (int64, error) {
	return s.store.BumpReadonlyRevocation(ctx, code)
}

// ReadonlyWatermark returns the room's revocation watermark.
func (s *Service) ReadonlyWatermark(ctx context.Context, code string) (int64, error) {
	_, _, revokedAt, err := s.store.VerifyKeyHash(ctx, code)
	return revokedAt, err
}

// Snapshot returns live shapes paged by id.
//
// A live room is asked first because it is authoritative and in memory; the store
// is the fallback for a room that was reaped but still exists on disk.
func (s *Service) Snapshot(ctx context.Context, code, after string, limit int) (*api.SnapshotPage, error) {
	if rh, err := s.hub.Get(code); err == nil {
		stats := rh.Stats()
		shapes, err := rh.SnapshotShapes(ctx)
		if err != nil {
			return nil, err
		}
		return pageShapes(shapes, stats.Seq, stats.Epoch, after, limit), nil
	}

	rec, err := s.store.LoadRoom(ctx, code)
	if err != nil {
		return nil, err
	}
	shapes := make([]protocol.Shape, 0, len(rec.Shapes))
	shapes = append(shapes, rec.Shapes...)
	if rec.Snapshot != nil && len(rec.Snapshot.Payload) > 0 {
		var snap []protocol.Shape
		if err := json.Unmarshal(rec.Snapshot.Payload, &snap); err == nil {
			shapes = snap
		}
	}
	epoch := rec.Epoch
	return pageShapes(shapes, rec.Seq, epoch, after, limit), nil
}

// pageShapes filters, sorts and slices a shape list for one page.
func pageShapes(shapes []protocol.Shape, seq, epoch int64, after string, limit int) *api.SnapshotPage {
	live := make([]protocol.Shape, 0, len(shapes))
	seen := make(map[string]struct{}, len(shapes))
	for _, sh := range shapes {
		if sh.Deleted {
			continue
		}
		if _, dup := seen[sh.ID]; dup {
			continue
		}
		seen[sh.ID] = struct{}{}
		live = append(live, sh)
	}
	// Paging is by id lexicographic order, which is the contract's stable cursor.
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })

	start := 0
	if after != "" {
		start = sort.Search(len(live), func(i int) bool { return live[i].ID > after })
	}
	end := start + limit
	if end > len(live) {
		end = len(live)
	}
	page := live[start:end]

	pageData := &api.SnapshotPage{
		Seq:      seq,
		Epoch:    epoch,
		Shapes:   make([]api.ShapeJSON, 0, len(page)),
		Complete: end >= len(live),
	}
	for i := range page {
		blob, err := json.Marshal(page[i])
		if err != nil {
			continue
		}
		pageData.Shapes = append(pageData.Shapes, blob)
	}
	if len(page) > 0 && !pageData.Complete {
		pageData.Next = page[len(page)-1].ID
	}
	return pageData
}

// SetTitle persists a room title (P1 room.meta write path).
func (s *Service) SetTitle(ctx context.Context, code, title string) error {
	title = protocol.TruncateTitle(title, s.cfg.Limits.MaxTitleRunes)
	return s.store.SetTitle(ctx, code, title)
}

// Backend reports the persistence backend for /readyz and /ops.
func (s *Service) Backend() string { return s.store.Backend() }

// Ping checks store connectivity for /readyz.
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }
