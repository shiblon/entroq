package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/shiblon/entroq/pkg/authz/meshpolicy"
)

// LoadFile installs an initial mesh document from path. It is intended for a
// projected ConfigMap that preserves the last operator-produced policy across
// service restarts.
func (a *Authorizer) LoadFile(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read mesh policy file: %w", err)
	}

	document, err := decodeDocument(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decode mesh policy file: %w", err)
	}
	if err := a.ReplaceMesh(ctx, document); err != nil {
		return fmt.Errorf("install mesh policy file: %w", err)
	}
	return nil
}

func decodeDocument(reader io.Reader) (meshpolicy.Document, error) {
	var document meshpolicy.Document
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return meshpolicy.Document{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return meshpolicy.Document{}, fmt.Errorf("multiple JSON values")
		}
		return meshpolicy.Document{}, err
	}
	return document, nil
}
