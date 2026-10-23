// Package oplog stores entities (issues, merge requests, users, ...) inside a
// git repository as append-only operation logs: one ref per entity, one
// commit per operation, with the operation stored as op.json in the commit's
// tree. This is the same family of design as Gerrit NoteDb and git-bug.
//
// Writes are compare-and-swap ref updates, so concurrent writers can never
// silently overwrite each other; callers serialise per repository and retry.
package oplog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlphaTechini/newgit/internal/gitx"
)

// Op is one operation on an entity.
type Op struct {
	Type string          `json:"type"`
	At   int64           `json:"at"`
	By   string          `json:"by"`
	Data json.RawMessage `json:"data,omitempty"`
	OID  string          `json:"-"` // commit that recorded the op
}

// Time returns the op timestamp.
func (o Op) Time() time.Time { return time.Unix(o.At, 0) }

// Decode unmarshals the op payload.
func (o Op) Decode(v any) error {
	if len(o.Data) == 0 {
		return nil
	}
	return json.Unmarshal(o.Data, v)
}

// NewOp builds an op with a payload.
func NewOp(typ, by string, data any) (Op, error) {
	op := Op{Type: typ, By: by, At: time.Now().Unix()}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return op, err
		}
		op.Data = b
	}
	return op, nil
}

// ErrConflict is returned when the entity moved during an append.
var ErrConflict = errors.New("oplog: concurrent update")

// Append records ops on the entity at ref. Each op becomes one commit.
// expectTip is the tip the caller last saw ("" for a new entity); pass
// "-" to append to whatever the current tip is.
func Append(ctx context.Context, gitDir, ref string, ops []Op, author gitx.Identity, expectTip string) (string, error) {
	cur, exists := gitx.RefTarget(ctx, gitDir, ref)
	if expectTip == "-" {
		expectTip = cur
	} else if expectTip != cur {
		return "", ErrConflict
	}
	if !exists {
		cur = ""
	}
	tip := cur
	for _, op := range ops {
		b, err := json.Marshal(op)
		if err != nil {
			return "", err
		}
		blob, err := gitx.HashObject(ctx, gitDir, b, nil)
		if err != nil {
			return "", err
		}
		tree, err := gitx.MkTree(ctx, gitDir, []string{"100644 blob " + blob + "\top.json"}, nil)
		if err != nil {
			return "", err
		}
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		msg := op.Type + "\n"
		c, err := gitx.CommitTree(ctx, gitDir, tree, parents, msg, author, []string{
			fmt.Sprintf("GIT_AUTHOR_DATE=%d +0000", op.At), fmt.Sprintf("GIT_COMMITTER_DATE=%d +0000", op.At),
		})
		if err != nil {
			return "", err
		}
		tip = c
	}
	if err := gitx.UpdateRef(ctx, gitDir, ref, tip, cur, nil); err != nil {
		if strings.Contains(err.Error(), "cannot lock ref") || strings.Contains(err.Error(), "but expected") {
			return "", ErrConflict
		}
		return "", err
	}
	return tip, nil
}

// Read returns all ops of an entity, oldest first.
func Read(ctx context.Context, gitDir, ref string) ([]Op, error) {
	tip, ok := gitx.RefTarget(ctx, gitDir, ref)
	if !ok {
		return nil, nil
	}
	return ReadFrom(ctx, gitDir, tip)
}

// ReadFrom reads ops reachable from a tip commit, oldest first.
func ReadFrom(ctx context.Context, gitDir, tip string) ([]Op, error) {
	out, err := gitx.Run(ctx, gitDir, nil, "rev-list", "--reverse", "--topo-order", "--end-of-options", tip)
	if err != nil {
		return nil, err
	}
	oids := strings.Fields(string(out))
	if len(oids) == 0 {
		return nil, nil
	}
	b, err := gitx.NewBatch(ctx, gitDir)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	ops := make([]Op, 0, len(oids))
	for _, oid := range oids {
		_, data, err := b.Get(oid + ":op.json")
		if err != nil {
			return nil, fmt.Errorf("oplog: read %s: %w", oid, err)
		}
		var op Op
		if err := json.Unmarshal(data, &op); err != nil {
			return nil, fmt.Errorf("oplog: decode %s: %w", oid, err)
		}
		op.OID = oid
		ops = append(ops, op)
	}
	return ops, nil
}
