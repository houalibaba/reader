package content

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

func TestSaveUserPreparedImagesEvenDistributionAndSparseIndex(t *testing.T) {
	mediaDir := t.TempDir()

	img1 := base64.StdEncoding.EncodeToString([]byte("image-payload-chapter-1"))
	img2 := base64.StdEncoding.EncodeToString([]byte("image-payload-chapter-2"))
	img3 := base64.StdEncoding.EncodeToString([]byte("image-payload-chapter-3"))
	img4 := base64.StdEncoding.EncodeToString([]byte("image-payload-chapter-4"))

	// 1. 4 unindexed chapter images distributed evenly across 8 sentences (2 lines per image)
	urls, err := SaveUserPreparedImages(mediaDir, "story_even", "story_even_v1", []CustomImageUpload{
		{Filename: "ch1.png", DataBase64: img1},
		{Filename: "ch2.png", DataBase64: img2},
		{Filename: "ch3.png", DataBase64: img3},
		{Filename: "ch4.png", DataBase64: img4},
	}, 8)
	if err != nil {
		t.Fatalf("SaveUserPreparedImages even failed: %v", err)
	}
	if len(urls) != 8 {
		t.Fatalf("expected 8 urls, got %d", len(urls))
	}
	if urls[0] != urls[1] || urls[2] != urls[3] || urls[4] != urls[5] || urls[6] != urls[7] {
		t.Fatalf("expected pairs of sentences to share chapter images: %v", urls)
	}
	if urls[0] == urls[2] || urls[2] == urls[4] || urls[4] == urls[6] {
		t.Fatalf("expected distinct chapter images across chapters: %v", urls)
	}

	// 2. Manual sparse per-sentence bindings (e.g. only sentence 1 and sentence 4 out of 6)
	sparseURLs, err := SaveUserPreparedImages(mediaDir, "story_sparse", "story_sparse_v1", []CustomImageUpload{
		{SentenceIndex: intPtr(1), Filename: "s02.png", DataBase64: img1},
		{SentenceIndex: intPtr(4), Filename: "s05.png", DataBase64: img2},
	}, 6)
	if err != nil {
		t.Fatalf("SaveUserPreparedImages sparse failed: %v", err)
	}
	if len(sparseURLs) != 6 {
		t.Fatalf("expected 6 urls, got %d", len(sparseURLs))
	}
	if sparseURLs[1] == "" || sparseURLs[4] == "" || sparseURLs[1] == sparseURLs[4] {
		t.Fatalf("expected distinct non-empty URLs at indices 1 and 4, got %v", sparseURLs)
	}
	for _, emptyIdx := range []int{0, 2, 3, 5} {
		if sparseURLs[emptyIdx] != "" {
			t.Fatalf("expected index %d to remain empty in sparse binding, got %q", emptyIdx, sparseURLs[emptyIdx])
		}
	}

	// 3. Full SHA-256 deduplication must NOT collide two different images that share the same length and 64-byte prefix
	sharedPrefix := bytes.Repeat([]byte("A"), 128)
	collisionCandA := base64.StdEncoding.EncodeToString(append(append([]byte{}, sharedPrefix...), []byte("DIFF_TAIL_1")...))
	collisionCandB := base64.StdEncoding.EncodeToString(append(append([]byte{}, sharedPrefix...), []byte("DIFF_TAIL_2")...))

	noCollideURLs, err := SaveUserPreparedImages(mediaDir, "story_sha", "story_sha_v1", []CustomImageUpload{
		{Filename: "a.png", DataBase64: collisionCandA},
		{Filename: "b.png", DataBase64: collisionCandB},
	}, 2)
	if err != nil {
		t.Fatalf("SaveUserPreparedImages sha256 failed: %v", err)
	}
	if noCollideURLs[0] == noCollideURLs[1] {
		t.Fatalf("false-positive deduplication collision: %v", noCollideURLs)
	}

	// 4. Reject unsafe path traversal story_id / version_id
	if _, err := SaveUserPreparedImages(mediaDir, "../escape", "v1", []CustomImageUpload{{Filename: "a.png", DataBase64: img1}}, 1); err == nil {
		t.Fatal("expected path traversal story_id to be rejected")
	}
	if _, err := SaveUserPreparedImages(mediaDir, filepath.Join("a", "b"), "v1", []CustomImageUpload{{Filename: "a.png", DataBase64: img1}}, 1); err == nil {
		t.Fatal("expected slash in story_id to be rejected")
	}
	if err := ValidateSafeIdentifier("story_id", "story_valid-01"); err != nil {
		t.Fatalf("expected valid identifier to pass, got %v", err)
	}
	if err := ValidateSafeIdentifier("story_id", strings.Repeat("a", 100)); err == nil {
		t.Fatal("expected overly long identifier to be rejected")
	}
}
