//go:build live

package poster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"
)

// Explicitly opt-in capability check: creates one post only after the community
// token itself has identified the configured group, and deletes only that post.
func TestLiveVKCommunityPosting(t *testing.T) {
	if os.Getenv("RUN_VK_COMMUNITY_PROBE") != "true" {
		t.Skip("community capability probe is opt-in")
	}
	token := os.Getenv("VK_TEST_COMMUNITY_TOKEN")
	group, e := strconv.ParseInt(os.Getenv("VK_TEST_GROUP_ID"), 10, 64)
	if token == "" || e != nil || group <= 0 {
		t.Fatal("VK_TEST_COMMUNITY_TOKEN and positive VK_TEST_GROUP_ID are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	api := NewAPI(Config{VKVersion: "5.199"})
	var permissions json.RawMessage
	if e = api.VK(ctx, "groups.getTokenPermissions", token, url.Values{}, &permissions); e != nil {
		t.Fatalf("community-token authorization check failed: %v", e)
	}
	var raw json.RawMessage
	// No group_ids: derive the community from the group token, not from a public lookup.
	if e = api.VK(ctx, "groups.getById", token, url.Values{}, &raw); e != nil {
		t.Fatalf("community identity lookup failed: %v", e)
	}
	type identity struct {
		ID int64 `json:"id"`
	}
	var groups []identity
	if json.Unmarshal(raw, &groups) != nil {
		var result struct {
			Groups []identity `json:"groups"`
		}
		if json.Unmarshal(raw, &result) != nil {
			t.Fatal("invalid community identity response")
		}
		groups = result.Groups
	}
	if len(groups) != 1 || groups[0].ID != group {
		t.Fatal("community token does not identify the configured test group; no post created")
	}
	nonce := make([]byte, 12)
	if _, e = rand.Read(nonce); e != nil {
		t.Fatal("cannot generate probe marker")
	}
	marker := "autoposter-ci-community-probe-" + hex.EncodeToString(nonce)
	var posted struct {
		ID int64 `json:"post_id"`
	}
	if e = api.VK(ctx, "wall.post", token, url.Values{"owner_id": {intString(-group)}, "from_group": {"1"}, "message": {marker}, "guid": {hex.EncodeToString(nonce)}}, &posted); e != nil {
		t.Fatalf("wall.post rejected the verified community token: %v; this is the actual publication method, not account.getAppPermissions", e)
	}
	if posted.ID <= 0 {
		t.Fatal("wall.post returned no post ID; inspect test wall for the probe marker")
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if e := api.VK(cleanup, "wall.delete", token, url.Values{"owner_id": {intString(-group)}, "post_id": {intString(posted.ID)}}, nil); e != nil {
			t.Errorf("wall.post succeeded but community-token cleanup failed: %v; remove only test post %d_%d manually", e, -group, posted.ID)
		}
	})
	t.Log("Verified community token created a real post; cleanup checks wall.delete")
}
