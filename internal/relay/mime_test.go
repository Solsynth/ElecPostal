package relay

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

type fakeAttachmentSource struct {
	files map[string]struct {
		data     []byte
		metadata AttachmentMetadata
	}
}

func (s fakeAttachmentSource) Open(_ context.Context, id string) (io.ReadCloser, AttachmentMetadata, error) {
	file, ok := s.files[id]
	if !ok {
		return nil, AttachmentMetadata{}, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(file.data)), file.metadata, nil
}

func TestRenderMessageIncludesReferencedAttachment(t *testing.T) {
	source := fakeAttachmentSource{files: map[string]struct {
		data     []byte
		metadata AttachmentMetadata
	}{"image": {data: []byte{1, 2, 3}, metadata: AttachmentMetadata{ID: "image", Filename: "image.png", MimeType: "image/png", Size: 3, ContentID: "logo", Disposition: "inline"}}}}
	data, err := renderMessage(context.Background(), Message{FromAddress: "a@example.test", To: []string{"b@example.test"}, Subject: "hello", Body: "<img src=\"cid:logo\">", ContentType: "text/html", AttachmentIDs: []string{"image"}}, source)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Content-Id: <logo>") || !strings.Contains(string(data), "AQID") {
		t.Fatalf("rendered message omitted inline attachment: %q", data)
	}
}

func TestRenderMessageRejectsMissingSource(t *testing.T) {
	_, err := renderMessage(context.Background(), Message{FromAddress: "a@example.test", To: []string{"b@example.test"}, AttachmentIDs: []string{"missing"}}, fakeAttachmentSource{files: map[string]struct {
		data     []byte
		metadata AttachmentMetadata
	}{}})
	if err == nil {
		t.Fatal("expected missing attachment error")
	}
}
