// postas posts a signed work as one of the dev accounts, through the same
// write lane the app uses: sign in, register a fresh device key, sign the
// work, POST /events.
//
// It exists so a second account can act while the Simulator is signed in as
// the first — a follower's stream frame, a mention's notification, a reply
// landing under a work — none of which one device can produce for itself.
// With -voice, -video or -image it uploads the file through the same media
// lane the composer uses first, so a voice note or a clip lands in the feed
// exactly as one posted from the app would.
//
//	go run ./cmd/postas -as tehanibentley -body "morning from the roof @dev #f33d3r"
//	go run ./cmd/postas -as miiyazuko -body "rooftop take" -voice ../seedmedia/rooftop.m4a -seconds 214
//	go run ./cmd/postas -as dev -body "a clip" -video ~/Movies/clip.mp4
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"f33d3r.com/ios/devserver/internal/api"
)

func main() {
	addr := flag.String("addr", "http://127.0.0.1:8081", "dev server")
	as := flag.String("as", "dev", "handle to post as")
	password := flag.String("password", "f33d3rdev", "that account's password")
	body := flag.String("body", "", "the work's body (required)")
	kind := flag.String("kind", "post", "post | reply")
	parent := flag.String("parent", "", "parent CID for a reply")
	voice := flag.String("voice", "", "an audio file to upload and post as a voice note")
	seconds := flag.Int("seconds", 0, "the voice note's length in whole seconds (the recorder's number; required with -voice)")
	video := flag.String("video", "", "a video file to upload, wait on, and post")
	image := flag.String("image", "", "an image file to upload and attach")
	flag.Parse()
	if *body == "" {
		log.Fatal("-body is required")
	}
	if *voice != "" && *seconds <= 0 {
		log.Fatal("-seconds is required with -voice: the server does not measure audio")
	}

	token, pial := login(*addr, *as, *password)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	device := api.DeviceKey{Key: key}
	if status, resp := call(*addr, "POST", "/api/pial/signing-key/register", token,
		mustJSON(map[string]string{"public_key_b64": device.SPKIBase64(), "algorithm": "ECDSA-P256"})); status != 200 {
		log.Fatalf("register key: %d %s", status, resp)
	}

	draft := api.WorkDraft{AuthorPIAL: pial, Kind: *kind, Body: *body, Tags: tagsIn(*body), ParentCID: *parent}
	if *image != "" {
		var uploaded struct {
			URL string `json:"url"`
		}
		upload(*addr, "/api/v1/media", "file", *image, token, http.StatusCreated, &uploaded)
		draft.MediaURLs = []string{uploaded.URL}
		fmt.Printf("image uploaded: %s\n", uploaded.URL)
	}
	if *voice != "" {
		var uploaded api.VoiceUploadDTO
		upload(*addr, "/api/v1/media/voice", "audio", *voice, token, http.StatusCreated, &uploaded)
		draft.VoiceURL = uploaded.URL
		draft.VoiceDurationSecs = *seconds
		fmt.Printf("voice uploaded: %s\n", uploaded.URL)
	}
	if *video != "" {
		job := waitForVideo(*addr, token, *video)
		draft.VideoMasterURL = job.MasterURL
		draft.VideoPosterURL = job.PosterURL
		draft.VideoWatermarkedURL = job.WatermarkedURL
		if job.DurationSecs != nil {
			draft.VideoDurationSecs = int(*job.DurationSecs + 0.5)
		}
		if job.Width != nil && job.Height != nil {
			draft.VideoWidth, draft.VideoHeight = *job.Width, *job.Height
		}
		fmt.Printf("video ready: %s (%dx%d, %ds)\n", job.MasterURL, draft.VideoWidth, draft.VideoHeight, draft.VideoDurationSecs)
	}

	envelope, cid, err := api.BuildEnvelope(device, draft)
	if err != nil {
		log.Fatal(err)
	}
	status, resp := call(*addr, "POST", "/events", token, envelope)
	if status != 201 && status != 200 {
		log.Fatalf("post: %d %s", status, resp)
	}
	var accepted struct {
		WorkID string `json:"work_id"`
	}
	_ = json.Unmarshal(resp, &accepted)
	fmt.Printf("posted as @%s: work %s cid %s\n", *as, accepted.WorkID, cid)
}

// waitForVideo uploads the file and polls the job the way the composer does,
// every second here rather than every five, until it settles. A duplicate is
// posted as a reference to the original, the composer's "Use anyway".
func waitForVideo(addr, token, path string) api.VideoJobDTO {
	var job api.VideoJobDTO
	status := uploadRaw(addr, "/api/v1/media/video", "file", path, token, &job)
	switch status {
	case http.StatusAccepted:
	case http.StatusOK:
		if job.Status == "duplicate" {
			fmt.Printf("video already on F33D3R as @%s's; posting a reference\n", job.DuplicateOfHandle)
			return job
		}
	default:
		log.Fatalf("upload video: %d %+v", status, job)
	}
	for i := 0; i < 120; i++ {
		time.Sleep(time.Second)
		st, resp := call(addr, "GET", "/api/v1/media/video/"+job.UploadID, token, nil)
		if st != 200 {
			log.Fatalf("video job: %d %s", st, resp)
		}
		if err := json.Unmarshal(resp, &job); err != nil {
			log.Fatal(err)
		}
		switch job.Status {
		case "ready":
			return job
		case "failed":
			log.Fatalf("video failed: %s", job.Error)
		}
	}
	log.Fatal("video never became ready")
	return job
}

func upload(addr, route, field, path, token string, want int, into any) {
	if status := uploadRaw(addr, route, field, path, token, into); status != want {
		log.Fatalf("upload %s: %d", route, status)
	}
}

// uploadRaw sends one file as the multipart field the route reads.
func uploadRaw(addr, route, field, path, token string, into any) int {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		log.Fatal(err)
	}
	part.Write(data)
	mw.Close()
	req, err := http.NewRequest("POST", addr+route, &buf)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		log.Printf("%s: %s", route, out)
	} else if err := json.Unmarshal(out, into); err != nil {
		log.Fatalf("%s: unreadable answer %s", route, out)
	}
	return resp.StatusCode
}

var tagRe = regexp.MustCompile(`#([A-Za-z0-9_]+)`)

// tagsIn mirrors the app's tag derivation: every #word in the body, lowered,
// in order, once.
func tagsIn(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range tagRe.FindAllStringSubmatch(body, -1) {
		t := strings.ToLower(m[1])
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func login(addr, handle, password string) (token, pial string) {
	status, resp := call(addr, "POST", "/api/v1/auth/login", "",
		mustJSON(map[string]string{"handle": handle, "password": password, "device_name": "postas"}))
	if status != 200 {
		log.Fatalf("login: %d %s", status, resp)
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(resp, &session); err != nil || session.Token == "" {
		log.Fatalf("login: no token in %s", resp)
	}
	status, resp = call(addr, "GET", "/api/v1/me", session.Token, nil)
	if status != 200 {
		log.Fatalf("me: %d %s", status, resp)
	}
	var me struct {
		PIAL string `json:"pial_id"`
	}
	_ = json.Unmarshal(resp, &me)
	if me.PIAL == "" {
		log.Fatal("me: no pial_id")
	}
	return session.Token, me.PIAL
}

func call(addr, method, path, token string, body []byte) (int, []byte) {
	req, err := http.NewRequest(method, addr+path, bytes.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		log.Fatal(err)
	}
	return b
}
