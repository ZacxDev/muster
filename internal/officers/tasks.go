package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// --- HQ's officer task read route ---------------------------------------------
//
// civ-hq internal/api (task-reconciler plan P20), read-only, for an officer
// whose type policy sets task_read: true:
//
//	GET /officers/{id}/tasks?open=&status=&since=&cursor=&limit=
//	    {rows: [{id, title, type, status, parent, assignees, owner, due,
//	     priority, created, updated, rev, last_comment_at, children_open,
//	     children_done}], next}
//	GET /officers/{id}/tasks/{task_id}
//	    {task, comments: [{id, author, body, created}], statuses, done_statuses}
//
// HQ never returns a private task (404 not_found, as for an absent one), and
// refuses a type without task_read (403 task_read_off). Both are 4xx, so they
// reach the instance as final refusals (HQError) and are never retried. A 429
// rate_limited is HQ's per-officer limit, shared with the obs route.
//
// 🔴 MUSTER BUILDS A URL HERE, SO IT DECODES STRICTLY. The obs tools pass
// their JSON body to HQ byte for byte and HQ's decoder refuses an unknown
// key. A query string has no such decoder on HQ's side for keys it does not
// read: an argument muster dropped, or a mistyped one, would silently be a
// filter the caller believes applied and HQ never saw. So the list tool takes
// exactly {open?, status?, since?, cursor?, limit?} with their JSON types, and
// refuses anything else before HQ is called. VALUES are HQ's to check (a since
// that is not RFC 3339, a limit out of range, an unknown status): muster only
// carries them.

// The task read tools. Offered to a task-reconciler officer only (ToolOffered).
const (
	ToolTaskList = "officer_task_list"
	ToolTaskGet  = "officer_task_get"
)

// TaskReconcilerOfficerType is the officer type whose instances read tasks.
const TaskReconcilerOfficerType = "task-reconciler"

// taskIDRe is an HQ task id (civ-hq's data row id): what may reach the URL
// path. Anything else is refused before HQ is called.
var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// taskListKeys are the list tool's arguments and the JSON type each takes.
var taskListKeys = map[string]string{"open": "boolean", "status": "string", "since": "string", "cursor": "string", "limit": "integer"}

// TaskListQuery turns the list tool's arguments into HQ's query string
// ("" or "?…"): strictly — see the top of this file.
func TaskListQuery(raw []byte) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var obj map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return "", fmt.Errorf("arguments must be one JSON object: {open?, status?, since?, cursor?, limit?}")
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	v := url.Values{}
	for _, k := range keys {
		typ, ok := taskListKeys[k]
		if !ok {
			return "", fmt.Errorf("%q is not an argument of %s: it takes open, status, since, cursor and limit only", k, ToolTaskList)
		}
		val := bytes.TrimSpace(obj[k])
		if string(val) == "null" {
			continue
		}
		switch typ {
		case "boolean":
			var b bool
			if json.Unmarshal(val, &b) != nil {
				return "", fmt.Errorf("%s must be true or false", k)
			}
			v.Set(k, strconv.FormatBool(b))
		case "integer":
			var n int64
			if json.Unmarshal(val, &n) != nil {
				return "", fmt.Errorf("%s must be an integer", k)
			}
			v.Set(k, strconv.FormatInt(n, 10))
		default:
			var s string
			if json.Unmarshal(val, &s) != nil {
				return "", fmt.Errorf("%s must be a string", k)
			}
			if s != "" {
				v.Set(k, s)
			}
		}
	}
	if len(v) == 0 {
		return "", nil
	}
	return "?" + v.Encode(), nil
}

// TaskGetID is the get tool's task id: {task_id} and nothing else, the id an
// HQ task id.
func TaskGetID(raw []byte) (string, error) {
	raw = bytes.TrimSpace(raw)
	var obj map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return "", fmt.Errorf("arguments must be one JSON object: {task_id}")
	}
	for k := range obj {
		if k != "task_id" {
			return "", fmt.Errorf("%q is not an argument of %s: it takes task_id only", k, ToolTaskGet)
		}
	}
	var id string
	if json.Unmarshal(obj["task_id"], &id) != nil || !taskIDRe.MatchString(id) {
		return "", fmt.Errorf("task_id must be an HQ task id (1-64 of A-Z, a-z, 0-9, _ and -)")
	}
	return id, nil
}

// TaskList is GET /officers/{id}/tasks with the officer token; query is
// TaskListQuery's.
func (c *Client) TaskList(ctx context.Context, query string) ([]byte, error) {
	if query != "" && !strings.HasPrefix(query, "?") {
		return nil, fmt.Errorf("officers: task list query must start with ?")
	}
	return c.do(ctx, http.MethodGet, c.officerPath()+"/tasks"+query, "", nil)
}

// TaskGet is GET /officers/{id}/tasks/{task_id} with the officer token; id
// must be TaskGetID's (checked again here: it goes into the path).
func (c *Client) TaskGet(ctx context.Context, id string) ([]byte, error) {
	if !taskIDRe.MatchString(id) {
		return nil, fmt.Errorf("officers: %q is not an HQ task id", id)
	}
	return c.do(ctx, http.MethodGet, c.officerPath()+"/tasks/"+url.PathEscape(id), "", nil)
}
