package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 is a local stand-in for one bucket: PutObject only, honouring
// If-None-Match: * the way S3 does (412 when the key exists).
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	headers []http.Header
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPut {
		http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
		return
	}
	f.headers = append(f.headers, r.Header.Clone())
	body, _ := io.ReadAll(r.Body)
	if _, ok := f.objects[r.URL.Path]; ok && r.Header.Get("If-None-Match") == "*" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusPreconditionFailed)
		io.WriteString(w, `<Error><Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message></Error>`)
		return
	}
	f.objects[r.URL.Path] = body
	w.Header().Set("ETag", `"x"`)
}

func newFakeS3(t *testing.T) (*fakeS3, *S3Uploader) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret"}, nil
		}),
	})
	return f, &S3Uploader{client: client, bucket: "bucket"}
}

func TestS3PutIsWriteOnce(t *testing.T) {
	f, up := newFakeS3(t)
	ctx := context.Background()
	if err := up.Put(ctx, "byname/a.db.gz", []byte("first"), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	h := f.headers[0]
	if h.Get("If-None-Match") != "*" {
		t.Errorf("If-None-Match = %q, want *", h.Get("If-None-Match"))
	}
	if h.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Errorf("SSE header = %q", h.Get("X-Amz-Server-Side-Encryption"))
	}
	if err := up.Put(ctx, "byname/a.db.gz", []byte("second"), "application/gzip"); !errors.Is(err, ErrExists) {
		t.Fatalf("second Put = %v, want ErrExists", err)
	}
	if got := string(f.objects["/bucket/byname/a.db.gz"]); !strings.Contains(got, "first") {
		t.Fatalf("object replaced: %q", got)
	}
}

func TestS3PutOtherErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
	}))
	defer srv.Close()
	_, up := newFakeS3(t)
	up.client = s3.New(up.client.Options(), func(o *s3.Options) { o.BaseEndpoint = aws.String(srv.URL) })
	err := up.Put(context.Background(), "byname/a.db.gz", []byte("x"), "application/gzip")
	if err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("Put = %v, want an access error", err)
	}
}

// The policy the docs tell you to attach to the host's key: it may only
// create objects under byname/, with SSE, and only with If-None-Match, so it
// can never replace an existing backup.
func TestIAMPolicyIsWriteOnce(t *testing.T) {
	b, err := os.ReadFile("../../deploy/aws/iam-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var pol struct {
		Statement []struct {
			Sid, Effect, Action, Resource string
			Condition                     map[string]map[string]string
		}
	}
	if err := json.Unmarshal(b, &pol); err != nil {
		t.Fatal(err)
	}
	var allows, denies int
	for _, st := range pol.Statement {
		if strings.Contains(st.Sid, "Ot") { // the project was once called ot
			t.Errorf("Sid %q is a leftover from the old name", st.Sid)
		}
		if st.Action != "s3:PutObject" {
			t.Errorf("%s: action %q", st.Sid, st.Action)
		}
		switch st.Effect {
		case "Allow":
			allows++
			if st.Resource != "arn:aws:s3:::BUCKET_NAME/byname/*" {
				t.Errorf("%s: resource %q", st.Sid, st.Resource)
			}
			if st.Condition["Null"]["s3:if-none-match"] != "false" {
				t.Errorf("%s: allows PutObject without If-None-Match: %v", st.Sid, st.Condition)
			}
			if st.Condition["StringEquals"]["s3:x-amz-server-side-encryption"] != "AES256" {
				t.Errorf("%s: allows PutObject without SSE: %v", st.Sid, st.Condition)
			}
		case "Deny":
			denies++
			if st.Condition["Null"]["s3:if-none-match"] != "true" || st.Resource != "arn:aws:s3:::BUCKET_NAME/*" {
				t.Errorf("%s: deny doesn't cover every put without If-None-Match: %v %s", st.Sid, st.Condition, st.Resource)
			}
		default:
			t.Errorf("%s: effect %q", st.Sid, st.Effect)
		}
	}
	if allows != 1 || denies != 1 {
		t.Errorf("want one Allow and one Deny, got %d and %d", allows, denies)
	}
}

func TestLifecycleKeepsReplacedVersionsAYear(t *testing.T) {
	b, err := os.ReadFile("../../deploy/aws/lifecycle.json")
	if err != nil {
		t.Fatal(err)
	}
	var lc struct {
		Rules []struct {
			NoncurrentVersionExpiration *struct{ NoncurrentDays int }
		}
	}
	if err := json.Unmarshal(b, &lc); err != nil {
		t.Fatal(err)
	}
	for _, r := range lc.Rules {
		if r.NoncurrentVersionExpiration != nil && r.NoncurrentVersionExpiration.NoncurrentDays < 365 {
			t.Errorf("noncurrent versions expire after %d days, want at least 365", r.NoncurrentVersionExpiration.NoncurrentDays)
		}
	}
}
