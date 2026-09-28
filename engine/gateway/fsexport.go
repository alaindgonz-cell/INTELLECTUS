package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// FSExportAdapter implements the external tool `export_view`: it writes an
// approved tree (read from the core, never from the model) into a new
// directory under Dir. It is idempotent per idempotency key: a completed
// export carries a marker file that Reconcile reads; an existing directory
// with a different marker is never overwritten.
type FSExportAdapter struct {
	Core *coreclient.Client
	Dir  string
}

const exportMarker = ".intellectus-export.json"

var targetRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}$`)

type exportArgs struct {
	ApprovedRoot string `json:"approved_root"`
	Target       string `json:"target"`
}

type exportMarkerFile struct {
	ApprovedRoot   string `json:"approved_root"`
	IdempotencyKey string `json:"idempotency_key"`
	Files          int    `json:"files"`
}

func (a *FSExportAdapter) parse(toolID string, raw json.RawMessage) (exportArgs, string, error) {
	var args exportArgs
	if toolID != "export_view" {
		return args, "", fmt.Errorf("fsexport: unsupported tool %q", toolID)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return args, "", fmt.Errorf("fsexport: bad arguments: %w", err)
	}
	if !strings.HasPrefix(args.ApprovedRoot, "sha256:") || !targetRe.MatchString(args.Target) || strings.Contains(args.Target, "..") {
		return args, "", errors.New("fsexport: invalid approved_root or target")
	}
	if a.Dir == "" {
		return args, "", errors.New("fsexport: no export directory configured")
	}
	return args, filepath.Join(a.Dir, args.Target), nil
}

// Execute writes the tree to a temporary directory and renames it into place.
func (a *FSExportAdapter) Execute(ctx context.Context, toolID string, raw json.RawMessage, key string) (Observation, error) {
	args, final, err := a.parse(toolID, raw)
	if err != nil {
		return Observation{Outcome: "FAILED", Data: map[string]any{"error": err.Error(), "executed": false}}, nil
	}
	if m, err := readMarker(final); err == nil {
		if m.IdempotencyKey == key && m.ApprovedRoot == args.ApprovedRoot {
			return Observation{Outcome: "SUCCEEDED", Data: map[string]any{"exported_root": m.ApprovedRoot, "path": final, "files": m.Files, "already_done": true}}, nil
		}
		return Observation{Outcome: "FAILED", Data: map[string]any{"error": "target exists with different content", "executed": false}}, nil
	} else if _, statErr := os.Stat(final); statErr == nil {
		return Observation{Outcome: "FAILED", Data: map[string]any{"error": "target exists and is not an INTELLECTUS export", "executed": false}}, nil
	}
	tree, err := a.Core.ReadTree(ctx, args.ApprovedRoot)
	if err != nil {
		return Observation{}, err // uncertain from the caller's view only if nothing was written; nothing was
	}
	if err := os.MkdirAll(a.Dir, 0o755); err != nil {
		return Observation{Outcome: "FAILED", Data: map[string]any{"error": err.Error(), "executed": false}}, nil
	}
	sum := sha256.Sum256([]byte(key))
	tmp := filepath.Join(a.Dir, ".tmp-"+hex.EncodeToString(sum[:8]))
	_ = os.RemoveAll(tmp)
	paths := make([]string, 0, len(tree.Files))
	for p := range tree.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if !safeRel(p) {
			_ = os.RemoveAll(tmp)
			return Observation{Outcome: "FAILED", Data: map[string]any{"error": "unsafe path in tree: " + p, "executed": false}}, nil
		}
		dst := filepath.Join(tmp, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return Observation{}, err
		}
		if err := os.WriteFile(dst, []byte(tree.Files[p]), 0o644); err != nil {
			return Observation{}, err
		}
	}
	mb, _ := json.Marshal(exportMarkerFile{ApprovedRoot: args.ApprovedRoot, IdempotencyKey: key, Files: len(paths)})
	if err := os.WriteFile(filepath.Join(tmp, exportMarker), mb, 0o644); err != nil {
		return Observation{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return Observation{}, err
	}
	return Observation{Outcome: "SUCCEEDED", Data: map[string]any{"exported_root": args.ApprovedRoot, "path": final, "files": len(paths)}}, nil
}

// Reconcile reads the marker: the effect happened iff a matching marker exists.
func (a *FSExportAdapter) Reconcile(ctx context.Context, toolID string, raw json.RawMessage, key string) (Finding, error) {
	args, final, err := a.parse(toolID, raw)
	if err != nil {
		return Finding{}, err
	}
	m, err := readMarker(final)
	if errors.Is(err, os.ErrNotExist) {
		no := false
		return Finding{EffectObserved: &no, Details: map[string]any{"path": final}}, nil
	}
	if err != nil {
		return Finding{Details: map[string]any{"error": err.Error()}}, nil
	}
	yes := m.IdempotencyKey == key && m.ApprovedRoot == args.ApprovedRoot
	if !yes {
		return Finding{Details: map[string]any{"error": "marker belongs to another export"}}, nil
	}
	return Finding{EffectObserved: &yes, Details: map[string]any{"exported_root": m.ApprovedRoot, "path": final, "files": m.Files}}, nil
}

func readMarker(dir string) (exportMarkerFile, error) {
	var m exportMarkerFile
	b, err := os.ReadFile(filepath.Join(dir, exportMarker))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
