package contracttest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"aa-server/internal/cryptocompat"
	"aa-server/internal/pulse"
)

type fixtures struct {
	Now    int64
	Fernet struct {
		Key       string
		Plaintext []byte
		Token     string
	}
	Password struct{ Password, Salt, Hash string }
	Daily    []struct {
		Key      []byte
		Now      int64
		Expected cryptocompat.DailyAccess
	}
	JWT struct {
		Seed, Public []byte
		Kid, Token   string
		Claims       map[string]any
	}
	Wire []struct {
		Name       string
		Status     int
		Body       []byte
		Headers    map[string]string
		State      string
		UID        *string
		Message    *[]byte
		GRPCStatus int `json:"grpc_status"`
		Error      bool
	}
	Trees []struct {
		Payload  []byte
		Expected map[string]any
	}
	Requests map[string][]byte
}

func load(t *testing.T) fixtures {
	t.Helper()
	data, err := os.ReadFile("testdata/python.json")
	if err != nil {
		t.Fatal(err)
	}
	var out fixtures
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPythonFernet(t *testing.T) {
	f := load(t)
	cipher, err := cryptocompat.NewFernet(f.Fernet.Key)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := cipher.Open(f.Fernet.Token)
	if err != nil || !bytes.Equal(plain, f.Fernet.Plaintext) {
		t.Fatalf("Python token: %v", err)
	}
	token, err := cipher.Seal(plain, time.Unix(f.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := cipher.Open(token)
	if err != nil || !bytes.Equal(plain, decoded) {
		t.Fatal("Go roundtrip failed")
	}
	raw, _ := base64.URLEncoding.DecodeString(token)
	raw[30] ^= 1
	if _, err = cipher.Open(base64.URLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered token accepted")
	}
}

func TestPythonPassword(t *testing.T) {
	f := load(t)
	hash, err := cryptocompat.PasswordHash(f.Password.Password, f.Password.Salt)
	if err != nil || hash != f.Password.Hash {
		t.Fatalf("password hash mismatch: %v", err)
	}
	if !cryptocompat.PasswordMatches(f.Password.Password, hash) || cryptocompat.PasswordMatches("wrong", hash) {
		t.Fatal("password verification failed")
	}
}

func TestPythonDailyAccess(t *testing.T) {
	for _, f := range load(t).Daily {
		actual := cryptocompat.Daily(f.Key, time.Unix(f.Now, 0))
		if actual != f.Expected {
			t.Fatalf("daily access mismatch at %d", f.Now)
		}
		if !cryptocompat.MatchDaily(actual.Password, actual) {
			t.Fatal("daily password rejected")
		}
	}
}

func TestPythonJWT(t *testing.T) {
	f := load(t)
	keys := map[string]ed25519.PublicKey{f.JWT.Kid: f.JWT.Public}
	if cryptocompat.KeyID(f.JWT.Public) != f.JWT.Kid {
		t.Fatal("key id changed")
	}
	claims, err := cryptocompat.Verify(f.JWT.Token, keys, "aa-id", "aa", time.Unix(f.Now, 0), true)
	if err != nil || claims["mirea"] != "synthetic-user" {
		t.Fatalf("Python JWT: %v", err)
	}
	token, err := cryptocompat.Sign(f.JWT.Seed, f.JWT.Claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cryptocompat.Verify(token, keys, "aa-id", "aa", time.Unix(f.Now, 0), true); err != nil {
		t.Fatal(err)
	}
	if _, err = cryptocompat.Verify(token, keys, "aa-id", "wrong", time.Unix(f.Now, 0), true); err == nil {
		t.Fatal("wrong audience accepted")
	}
	if _, err = cryptocompat.Verify(token, keys, "aa-id", "aa", time.Unix(f.Now+600, 0), true); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestPythonWire(t *testing.T) {
	for _, f := range load(t).Wire {
		t.Run(f.Name, func(t *testing.T) {
			headers := http.Header{}
			for k, v := range f.Headers {
				headers.Set(k, v)
			}
			message, status, err := pulse.Message(f.Body, headers)
			if (err != nil) != f.Error {
				t.Fatalf("decode error mismatch: %v", err)
			}
			if !f.Error {
				if status != f.GRPCStatus {
					t.Fatal("gRPC status mismatch")
				}
				if f.Message == nil {
					if message != nil {
						t.Fatal("expected no message")
					}
				} else if !bytes.Equal(message, *f.Message) {
					t.Fatal("message changed")
				}
			}
			state, uid := pulse.AuthenticationState(f.Status, headers, f.Body)
			expectedUID := ""
			if f.UID != nil {
				expectedUID = *f.UID
			}
			if state != f.State || uid != expectedUID {
				t.Fatalf("identity mismatch: %s %s", state, uid)
			}
		})
	}
}

func TestPythonDiagnosticTrees(t *testing.T) {
	for i, f := range load(t).Trees {
		data, err := json.Marshal(pulse.DecodeTree(f.Payload))
		if err != nil {
			t.Fatal(err)
		}
		var normalized map[string]any
		if err = json.Unmarshal(data, &normalized); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalized, f.Expected) {
			t.Fatalf("diagnostic tree mismatch at fixture %d", i)
		}
	}
}

func TestPythonRequests(t *testing.T) {
	expected := load(t).Requests
	if !bytes.Equal(pulse.ProfileRequest(), expected["profile"]) {
		t.Fatal("profile request changed")
	}
	if !bytes.Equal(pulse.ACSTimelineRequest("synthetic-user", 1000, 2000), expected["acs"]) {
		t.Fatal("ACS request changed")
	}
	if !bytes.Equal(pulse.LessonAttendanceRequest("lesson", "log"), expected["attendance"]) {
		t.Fatal("attendance request changed")
	}
}
