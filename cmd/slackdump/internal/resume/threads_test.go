// Copyright (c) 2021-2026 Rustam Gilyazov and Contributors.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package resume

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/rusq/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/rusq/slackdump/v4/cmd/slackdump/internal/bootstrap"
	"github.com/rusq/slackdump/v4/cmd/slackdump/internal/cfg"
	"github.com/rusq/slackdump/v4/internal/chunk"
	"github.com/rusq/slackdump/v4/internal/chunk/backend/dbase"
	"github.com/rusq/slackdump/v4/internal/client"
	"github.com/rusq/slackdump/v4/internal/network"
	"github.com/rusq/slackdump/v4/internal/structures"
	"github.com/rusq/slackdump/v4/stream"
)

func Test_newSearchThreadWindow(t *testing.T) {
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		start     time.Time
		end       time.Time
		wantStart time.Time
		wantEnd   time.Time
		wantErr   string
	}{
		{
			name:      "aligned end normalized to UTC",
			start:     time.Date(2026, 11, 12, 2, 2, 3, 1000, time.FixedZone("+01", 3600)),
			end:       day.Add(time.Microsecond).In(time.FixedZone("+02", 7200)),
			wantStart: time.Date(2026, 11, 12, 1, 2, 3, 1000, time.UTC),
			wantEnd:   day.Add(time.Microsecond),
		},
		{
			name: "unaligned UTC end", start: day,
			end: day.Add(1500 * time.Nanosecond), wantStart: day, wantEnd: day.Add(2 * time.Microsecond),
		},
		{
			name: "sub-microsecond interval", start: day.Add(1400 * time.Nanosecond),
			end: day.Add(1500 * time.Nanosecond), wantStart: day.Add(1400 * time.Nanosecond), wantEnd: day.Add(2 * time.Microsecond),
		},
		{name: "missing", end: day, wantErr: "specify -time-from"},
		{
			name: "equal to unrounded end", start: day.Add(1500 * time.Nanosecond),
			end: day.Add(1500 * time.Nanosecond), wantErr: "must be earlier",
		},
		{
			name: "later than unrounded end", start: day.Add(1600 * time.Nanosecond),
			end: day.Add(1500 * time.Nanosecond), wantErr: "must be earlier",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := newSearchThreadWindow(tc.start, tc.end)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantStart, w.from)
			assert.Equal(t, tc.wantEnd, w.to)
		})
	}
}

func Test_parseSearchThreadRoot(t *testing.T) {
	const parent, reply = "1794509982.127029", "1794509984.000017"
	hitTS, err := structures.ParseCanonicalSlackTS(reply)
	require.NoError(t, err)
	valid := searchHit("C123", parent, reply)
	base := strings.Split(valid.Permalink, "?")[0]
	for _, tc := range []struct {
		name string
		link string
		ok   bool
	}{
		{"escaped timestamp and optional channel", base + "?cid=C123&thread_ts=1794509982%2E127029&x=y", true},
		{"missing root", base, false},
		{"wrong path channel", strings.Replace(valid.Permalink, "/C123/", "/C456/", 1), false},
		{"wrong path timestamp", strings.Replace(valid.Permalink, "p1794509984000017", "p1", 1), false},
		{"wrong query channel", valid.Permalink + "&cid=C456", false},
		{"multiple timestamps", valid.Permalink + "&thread_ts=" + reply, false},
		{"malformed timestamp", base + "?thread_ts=1794509982.127", false},
		{"parent newer than hit", base + "?thread_ts=1794509984.000018", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := valid
			hit.Permalink = tc.link
			root, stamp, ok := parseSearchThreadRoot(hit, hitTS)
			require.Equal(t, tc.ok, ok)
			if ok {
				assert.Equal(t, structures.SlackLink{Channel: "C123", ThreadTS: parent}, root)
				assert.Equal(t, parent, structures.FormatSlackTS(stamp))
			}
		})
	}
}

func Test_searchThreadChannels(t *testing.T) {
	boundary := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	history := map[string]time.Time{
		"CEMPTY": {}, "CINCOMPLETE": {},
		"CSTILL": boundary, "CSTALE": boundary.Add(-time.Second),
		"COVERRIDE": boundary.Add(-time.Second), "CSKIP": boundary,
	}
	explicit, err := structures.NewEntityList([]string{"COVERRIDE,2021-01-01", "CNEW", "^CSKIP,2020-01-01", "CROOT:1700000000.000001,2019-01-01"})
	require.NoError(t, err)
	result, channels, start := searchThreadChannels(history, 24*time.Hour, &boundary, explicit)
	assert.Equal(t, map[string]struct{}{"CEMPTY": {}, "CINCOMPLETE": {}, "CSTILL": {}, "COVERRIDE": {}, "CNEW": {}}, channels)
	assert.Equal(t, 2, result.skippedStale)
	still, ok := result.list.Get("CSTILL")
	require.True(t, ok)
	assert.Equal(t, boundary.Add(-24*time.Hour), still.Oldest)
	empty, ok := result.list.Get("CEMPTY")
	require.True(t, ok)
	assert.True(t, empty.Oldest.IsZero())
	explicitChannel, ok := result.list.Get("COVERRIDE")
	require.True(t, ok)
	assert.Equal(t, "2021-01-01", explicitChannel.Oldest.Format(time.DateOnly))
	assert.Equal(t, explicitChannel.Oldest, start)

	t.Run("bare channel replaces the stored start", func(t *testing.T) {
		history := map[string]time.Time{
			"CBARE": boundary, "CLATER": boundary.Add(24 * time.Hour), "CSTALE": boundary.Add(-time.Second),
		}
		explicit, err := structures.NewEntityList([]string{"CBARE"})
		require.NoError(t, err)
		result, channels, start := searchThreadChannels(history, 48*time.Hour, &boundary, explicit)
		assert.Equal(t, map[string]struct{}{"CBARE": {}, "CLATER": {}}, channels)
		assert.Equal(t, boundary.Add(-24*time.Hour), start)
		bare, ok := result.list.Get("CBARE")
		require.True(t, ok)
		assert.True(t, bare.Oldest.IsZero())
	})
}

func Test_decideSearchResume(t *testing.T) {
	excludes, err := structures.NewEntityList([]string{"^CNEVER"})
	require.NoError(t, err)
	assert.Equal(t, resumeDecisionNoop, decideSearchResume(latestResult{list: excludes, hasSourceData: true}, false))
	assert.Equal(t, resumeDecisionContinue, decideSearchResume(latestResult{list: excludes, hasSourceData: true}, true))
	empty := structures.NewEntityListFromItems()
	assert.Equal(t, resumeDecisionNoop, decideSearchResume(latestResult{list: empty, hasSourceData: true, skippedStale: 2}, false))
	assert.Equal(t, resumeDecisionContinue, decideSearchResume(latestResult{list: empty, hasSourceData: true, skippedStale: 2}, true))
	assert.Equal(t, resumeDecisionInvalidArchive, decideSearchResume(latestResult{list: empty}, false))
}

func Test_searchThreadWindow_addThreads(t *testing.T) {
	w, err := newSearchThreadWindow(time.Date(2026, 11, 1, 1, 2, 3, 1000, time.UTC), time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	channels := map[string]struct{}{"CKNOWN": {}}
	const old, explicit, excluded = "1700000000.000001", "1700000001.300000", "1700000002.000001"
	const fullQuery = "is:thread after:2026-10-30 before:2026-11-11"
	const leftQuery = "is:thread after:2026-10-30 before:2026-11-07"
	const rightQuery = "is:thread after:2026-11-03 before:2026-11-11"
	lower, end := structures.FormatSlackTS(w.from), structures.FormatSlackTS(w.to)
	mid := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	split := structures.FormatSlackTS(mid)
	t.Run("uncapped full window reuses the first page", func(t *testing.T) {
		const nextRoot = "1700000005.000001"
		list, err := structures.NewEntityList([]string{"CKNOWN", "CKNOWN:1700000001.3,2023-01-01", "^CKNOWN:" + excluded})
		require.NoError(t, err)
		before := structures.FormatSlackTS(w.from.Add(-time.Microsecond))
		var cursors []string
		api := testResumeClient(func(method string, values url.Values) (jsonObject, error) {
			assert.Equal(t, "search.messages", method)
			assert.Equal(t, fullQuery, values.Get("query"))
			cursor := values.Get("cursor")
			cursors = append(cursors, cursor)
			switch cursor {
			case "*":
				page := searchPage("next", 7, searchHit("CKNOWN", old, lower), searchHit("CKNOWN", explicit, lower),
					searchHit("CKNOWN", excluded, lower), searchHit("CKNOWN", "1700000003.000001", before))
				page["messages"].(jsonObject)["paging"] = jsonObject{"pages": 9}
				return page, nil
			case "next":
				return searchPage("", 10_000, searchHit("CKNOWN", nextRoot, split), searchHit("COTHER", old, split),
					searchHit("CKNOWN", "1700000004.000001", end)), nil
			}
			return nil, fmt.Errorf("unexpected search cursor %q", cursor)
		})
		_, err = w.addThreads(t.Context(), stream.New(api, network.NoLimits), list, channels)
		require.NoError(t, err)
		assert.Equal(t, []string{"*", "next"}, cursors)
		require.Len(t, list.Index(), 5)
		_, ok := list.Get("CKNOWN:" + nextRoot)
		assert.True(t, ok)
		root, ok := list.Get("CKNOWN:" + old)
		require.True(t, ok)
		assert.Equal(t, "1700000000.000000", structures.FormatSlackTS(root.Oldest))
		assert.Equal(t, end, structures.FormatSlackTS(root.Latest))
		dir, ok := list.Get("CKNOWN:1700000001.3")
		require.True(t, ok)
		assert.Equal(t, "2023-01-01", dir.Oldest.Format(time.DateOnly))
	})

	for _, tc := range []struct {
		name       string
		total      int
		paging     jsonObject
		pagination jsonObject
		failRight  bool
	}{
		{name: "first-page message total", total: 10_000},
		{name: "first-page paging total", paging: jsonObject{"total": 10_001}},
		{name: "first-page pagination total", pagination: jsonObject{"total_count": 10_000}},
		{name: "first-page paging pages", paging: jsonObject{"pages": 100}},
		{name: "first-page pagination pages", pagination: jsonObject{"page_count": 101}},
		{name: "second child failure leaves the list unchanged", total: 10_000, failRight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := structures.NewEntityList([]string{"CKNOWN"})
			require.NoError(t, err)
			const left, right, padded, parentOnly = "1700000003.000001", "1700000004.000001", "1700000005.000001", "1700000006.000001"
			beforeSplit := structures.FormatSlackTS(mid.Add(-time.Microsecond))
			var queries []string
			api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
				assert.Equal(t, "search.messages", method)
				query := args.Get("query")
				queries = append(queries, query)
				if cursor := args.Get("cursor"); cursor != "*" {
					return nil, fmt.Errorf("unexpected search cursor %q", cursor)
				}
				switch query {
				case fullQuery:
					page := searchPage("do-not-follow", tc.total, searchHit("CKNOWN", parentOnly, lower))
					messages := page["messages"].(jsonObject)
					messages["paging"] = tc.paging
					for key, value := range tc.pagination {
						messages["pagination"].(jsonObject)[key] = value
					}
					return page, nil
				case leftQuery:
					return searchPage("", 3, searchHit("CKNOWN", old, lower), searchHit("CKNOWN", left, beforeSplit),
						searchHit("CKNOWN", padded, split)), nil
				case rightQuery:
					if tc.failRight {
						return jsonObject{"ok": false, "error": "invalid_auth"}, nil
					}
					return searchPage("", 4, searchHit("CKNOWN", old, split), searchHit("CKNOWN", right, split),
						searchHit("CKNOWN", padded, beforeSplit), searchHit("CKNOWN", padded, end)), nil
				}
				return nil, fmt.Errorf("unexpected search query %q", query)
			})
			opt, err := w.addThreads(t.Context(), stream.New(api, network.NoLimits), list, channels)
			assert.Equal(t, []string{fullQuery, leftQuery, rightQuery}, queries)
			if tc.failRight {
				require.ErrorContains(t, err, "thread search [2026-11-05T00:00:00Z, 2026-11-08T18:00:00Z)")
				assert.ErrorContains(t, err, "invalid_auth")
				assert.Nil(t, opt)
				assert.Len(t, list.Index(), 1)
				return
			}
			require.NoError(t, err)
			require.Len(t, list.Index(), 4)
			for _, ts := range []string{old, left, right} {
				root, ok := list.Get("CKNOWN:" + ts)
				require.True(t, ok, "root %s", ts)
				assert.Equal(t, w.to, root.Latest)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		terminal bool
	}{
		{name: "missing first-page counts and nonterminal page 100"},
		{name: "low first-page counts and terminal page 100", terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := structures.NewEntityList([]string{"CKNOWN"})
			require.NoError(t, err)
			var parentPages int
			var children []string
			api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
				assert.Equal(t, "search.messages", method)
				cursor := args.Get("cursor")
				switch query := args.Get("query"); query {
				case fullQuery:
					parentPages++
					switch parentPages {
					case 1:
						assert.Equal(t, "*", cursor)
						page := searchPage("2", 1, searchHit("CKNOWN", old, lower))
						if !tc.terminal {
							delete(page["messages"].(jsonObject), "total")
						}
						return page, nil
					case 100:
						if tc.terminal {
							return searchPage("", 0), nil
						}
					case 101:
						return nil, fmt.Errorf("search requested page 101")
					}
					assert.Equal(t, fmt.Sprint(parentPages), cursor)
					return searchPage(fmt.Sprint(parentPages+1), 10_000), nil
				case leftQuery, rightQuery:
					children = append(children, query)
					assert.Equal(t, "*", cursor)
					return searchPage("", 0), nil
				}
				return nil, fmt.Errorf("unexpected search query %q", args.Get("query"))
			})
			_, err = w.addThreads(t.Context(), stream.New(api, network.NoLimits), list, channels)
			require.NoError(t, err)
			assert.Equal(t, 100, parentPages)
			assert.Equal(t, []string{leftQuery, rightQuery}, children)
			assert.Len(t, list.Index(), 1, "discard the capped parent's candidate")
		})
	}

	t.Run("capped window without an interior midnight", func(t *testing.T) {
		window := searchThreadWindow{from: mid, to: mid.AddDate(0, 0, 1)}
		list, err := structures.NewEntityList([]string{"CKNOWN"})
		require.NoError(t, err)
		api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
			assert.Equal(t, "search.messages", method)
			if args.Get("cursor") != "*" {
				return nil, fmt.Errorf("unexpected search cursor %q", args.Get("cursor"))
			}
			return searchPage("do-not-follow", 10_000), nil
		})
		_, err = window.addThreads(t.Context(), stream.New(api, network.NoLimits), list, channels)
		require.ErrorContains(t, err, "thread search [2026-11-05T00:00:00Z, 2026-11-06T00:00:00Z)")
		assert.ErrorContains(t, err, "10000 matches")
		assert.ErrorContains(t, err, "no interior UTC midnight")
		assert.Len(t, list.Index(), 1)
	})

	t.Run("unaligned exclusive end includes the preceding Slack timestamp", func(t *testing.T) {
		day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
		window, err := newSearchThreadWindow(day, day.Add(1500*time.Nanosecond))
		require.NoError(t, err)
		list, err := structures.NewEntityList([]string{"CKNOWN"})
		require.NoError(t, err)
		before := structures.FormatSlackTS(day.Add(time.Microsecond))
		after := structures.FormatSlackTS(day.Add(2 * time.Microsecond))
		api := testResumeClient(func(method string, _ url.Values) (jsonObject, error) {
			assert.Equal(t, "search.messages", method)
			return searchPage("", 2, searchHit("CKNOWN", old, before), searchHit("CKNOWN", explicit, after)), nil
		})
		_, err = window.addThreads(t.Context(), stream.New(api, network.NoLimits), list, channels)
		require.NoError(t, err)
		require.Equal(t, 2, list.IncludeCount())
		root, ok := list.Get("CKNOWN:" + old)
		require.True(t, ok)
		assert.Equal(t, after, structures.FormatSlackTS(root.Latest))
	})

	t.Run("short search warns and keeps discovered roots", func(t *testing.T) {
		list, err := structures.NewEntityList([]string{"CKNOWN"})
		require.NoError(t, err)
		var logs bytes.Buffer
		prevLog := cfg.Log
		t.Cleanup(func() { cfg.Log = prevLog })
		cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))

		var calls int
		api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
			assert.Equal(t, "search.messages", method)
			assert.Equal(t, fullQuery, args.Get("query"))
			assert.Equal(t, "*", args.Get("cursor"))
			calls++
			page := searchPage("", 1, searchHit("CKNOWN", old, lower))
			page["messages"].(jsonObject)["paging"] = jsonObject{"total": 3}
			page["messages"].(jsonObject)["pagination"] = jsonObject{"total_count": 2}
			return page, nil
		})
		opt, err := w.addThreads(t.Context(), stream.New(api, network.NoLimits), list, channels)
		require.NoError(t, err)
		assert.NotNil(t, opt)
		assert.Equal(t, 1, calls)
		assert.Equal(t, 2, list.IncludeCount())
		_, ok := list.Get("CKNOWN:" + old)
		assert.True(t, ok)
		assert.Contains(t, logs.String(), `level=WARN msg="Slack search returned fewer matches than reported (2 out of 3 results missing). Missing hits can belong to threads already identified."`)
		assert.Contains(t, logs.String(), "from=2026-11-01T01:02:03.000Z to=2026-11-08T18:00:00.000Z total=3 raw_matches=1")
	})

	for _, tc := range []struct {
		name    string
		handler func(url.Values) jsonObject
		error   string
	}{
		{"repeated cursor", func(url.Values) jsonObject { return searchPage("same", 0) }, "repeated search cursor"},
		{"repeated cursor at cap", func(url.Values) jsonObject { return searchPage("*", 10_000) }, "repeated search cursor"},
		{"Slack error", func(url.Values) jsonObject { return jsonObject{"ok": false, "error": "invalid_auth"} }, "invalid_auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := structures.NewEntityList([]string{"CKNOWN"})
			require.NoError(t, err)
			api := testResumeClient(func(method string, values url.Values) (jsonObject, error) {
				assert.Equal(t, "search.messages", method)
				return tc.handler(values), nil
			})
			_, err = w.addThreads(t.Context(), stream.New(api, network.NoLimits), list, channels)
			require.ErrorContains(t, err, tc.error)
			assert.Equal(t, 1, list.IncludeCount())
		})
	}
}

type jsonObject map[string]any

type resumeRoundTripper func(*http.Request) (*http.Response, error)

func (f resumeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func testResumeClient(handler func(string, url.Values) (jsonObject, error)) client.Slack {
	return testResumeClientWithContext(func(_ context.Context, method string, args url.Values) (jsonObject, error) {
		return handler(method, args)
	})
}

func testResumeClientWithContext(handler func(context.Context, string, url.Values) (jsonObject, error)) client.Slack {
	return client.Wrap(slack.New("test-token", slack.OptionHTTPClient(&http.Client{Transport: resumeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		payload, err := handler(r.Context(), path.Base(r.URL.Path), r.Form)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		headers := http.Header{"Content-Type": {"application/json"}}
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})})))
}

func searchHit(channelID, parentTS, hitTS string) slack.SearchMessage {
	return slack.SearchMessage{
		Timestamp: hitTS, Channel: slack.CtxChannel{ID: channelID},
		Permalink: fmt.Sprintf("https://test.slack.com/archives/%s/p%s?thread_ts=%s", channelID, strings.Replace(hitTS, ".", "", 1), parentTS),
	}
}

func searchPage(cursor string, total int, matches ...slack.SearchMessage) jsonObject {
	return jsonObject{"ok": true, "messages": jsonObject{
		"pagination": jsonObject{"next_cursor": cursor}, "total": total, "matches": matches,
	}}
}

func testArchive(t *testing.T, chunks ...chunk.Chunk) (string, *sqlx.DB) {
	t.Helper()
	dir := t.TempDir()
	conn, err := bootstrap.Database(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	db, err := dbase.New(t.Context(), conn, dbase.SessionInfo{Mode: "archive"})
	require.NoError(t, err)
	require.NoError(t, db.Encode(t.Context(), &chunk.Chunk{
		Type: chunk.CWorkspaceInfo, WorkspaceInfo: &slack.AuthTestResponse{TeamID: "TTEST"},
	}))
	for _, c := range chunks {
		require.NoError(t, db.Encode(t.Context(), &c))
	}
	require.NoError(t, db.Finish())
	return dir, conn
}

func configureSearchResume(t *testing.T) {
	t.Helper()
	prevFlags, prevLimits := resumeFlags, cfg.Limits
	prevOldest, prevLatest := cfg.Oldest, cfg.Latest
	prevFiles, prevAvatars := cfg.WithFiles, cfg.WithAvatars
	t.Cleanup(func() {
		resumeFlags, cfg.Limits = prevFlags, prevLimits
		cfg.Oldest, cfg.Latest = prevOldest, prevLatest
		cfg.WithFiles, cfg.WithAvatars = prevFiles, prevAvatars
	})
	cfg.Limits = network.NoLimits
	cfg.Limits.Tier2.Retries, cfg.Limits.Tier3.Retries = 1, 1
	require.NoError(t, cfg.Oldest.Set("2026-11-12"))
	cfg.Latest = cfg.TimeValue(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))
	cfg.WithFiles, cfg.WithAvatars = false, false
	resumeFlags.SearchThread, resumeFlags.IncludeThreads = true, true
}

func testResumeStarter(api client.Slack) func(context.Context, ...client.Option) (client.Slack, error) {
	return func(context.Context, ...client.Option) (client.Slack, error) { return api, nil }
}

func commonResumeMethod(method string, args url.Values) jsonObject {
	switch method {
	case "auth.test":
		return jsonObject{"ok": true, "team_id": "TTEST", "user_id": "UTEST"}
	case "users.list":
		return jsonObject{"ok": true, "members": []slack.User{{ID: "UTEST", Name: "archiver"}}}
	case "conversations.info":
		return jsonObject{"ok": true, "channel": structures.ChannelFromID(args.Get("channel"))}
	case "conversations.members":
		return jsonObject{"ok": true, "members": []string{"UTEST"}}
	default:
		return nil
	}
}

func testSlackMessage(ts, parent, content, fileID string) slack.Message {
	m := slack.Message{Msg: slack.Msg{Type: "message", Channel: "", Timestamp: ts, ThreadTimestamp: parent, Text: content, User: "UTEST"}}
	if fileID != "" {
		m.Files = []slack.File{{ID: fileID, Mode: "hosted"}}
	}
	return m
}

type resumeCanvasClient struct {
	client.Slack
	roots []slack.Message
}

func (*resumeCanvasClient) CanvasSupported() bool { return true }

func (c *resumeCanvasClient) CanvasThreadRoots(context.Context, string) ([]slack.Message, error) {
	return c.roots, nil
}

func Test_runResumeWithClient(t *testing.T) {
	const parent = "1700000000.000001"
	const known = "1794488400.000004"
	const reply = "1794628800.000010"
	const reply2 = "1794715200.000011"

	for _, tc := range []struct {
		name          string
		lookback      string
		wantStart     time.Time
		wantSearchDay string
	}{
		{"default lookback", "", time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC), "2026-11-07"},
		{"28-day lookback", "p28d", time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC), "2026-10-17"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configureSearchResume(t)
			cfg.Oldest = cfg.TimeValue(time.Time{})
			if tc.lookback != "" {
				resumeFlags.Lookback = new(extDuration)
				require.NoError(t, resumeFlags.Lookback.Set(tc.lookback))
			}
			completed := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
			archived := testSlackMessage(structures.FormatSlackTS(completed), "", "completed history", "")
			dir, conn := testArchive(t,
				chunk.Chunk{Type: chunk.CMessages, ChannelID: "COWNER", IsLast: true, Messages: []slack.Message{archived}, Count: 1},
				chunk.Chunk{Type: chunk.CMessages, ChannelID: "CEMPTY", IsLast: true},
			)
			start := structures.FormatSlackTS(tc.wantStart)
			before := structures.FormatSlackTS(tc.wantStart.Add(-time.Microsecond))
			root := testSlackMessage(parent, parent, "old parent in undated channel", "")
			boundaryReply := testSlackMessage(start, parent, "reply at the inclusive start", "")
			api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
				switch method {
				case "search.messages":
					assert.Equal(t, "is:thread after:"+tc.wantSearchDay+" before:2026-11-21", args.Get("query"))
					return searchPage("", 2, searchHit("CEMPTY", "1700000001.000002", before), searchHit("CEMPTY", parent, start)), nil
				case "conversations.history":
					if args.Get("channel") == "COWNER" {
						assert.Equal(t, start, args.Get("oldest"))
					} else {
						assert.Empty(t, args.Get("oldest"))
					}
					return historyPage(""), nil
				case "conversations.replies":
					assert.Equal(t, "CEMPTY", args.Get("channel"))
					assert.Equal(t, parent, args.Get("ts"))
					return historyPage("", root, boundaryReply), nil
				default:
					return commonResumeMethod(method, args), nil
				}
			})
			require.NoError(t, runResumeWithClient(t.Context(), CmdResume, []string{dir}, testResumeStarter(api)))
			var saved []string
			require.NoError(t, conn.Select(&saved, `
SELECT M.TXT FROM MESSAGE M JOIN CHUNK C ON C.ID = M.CHUNK_ID
WHERE C.TYPE_ID = ? ORDER BY M.TXT`, chunk.CThreadMessages))
			assert.Equal(t, []string{root.Text, boundaryReply.Text}, saved)
		})
	}

	t.Run("undated search scope requires a start before client setup", func(t *testing.T) {
		configureSearchResume(t)
		cfg.Oldest = cfg.TimeValue(time.Time{})
		dir, conn := testArchive(t, chunk.Chunk{Type: chunk.CMessages, ChannelID: "CEMPTY", IsLast: true})
		startClient := func(context.Context, ...client.Option) (client.Slack, error) {
			t.Fatal("unexpected Slack client setup")
			return nil, nil
		}
		err := runResumeWithClient(t.Context(), CmdResume, []string{dir}, startClient)
		require.ErrorContains(t, err, "specify -time-from")
		assert.Equal(t, []bool{true}, sessionFinishes(t, conn))
	})

	t.Run("direct roots only need no search start", func(t *testing.T) {
		configureSearchResume(t)
		cfg.Oldest = cfg.TimeValue(time.Time{})
		dir, conn := testArchive(t)
		root := testSlackMessage(parent, parent, "explicit root", "")
		api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
			switch method {
			case "conversations.replies":
				assert.Equal(t, parent, args.Get("ts"))
				assert.Empty(t, args.Get("oldest"))
				assert.Equal(t, structures.FormatSlackTS(time.Time(cfg.Latest)), args.Get("latest"))
				return historyPage("", root), nil
			default:
				if payload := commonResumeMethod(method, args); payload != nil {
					return payload, nil
				}
				return nil, fmt.Errorf("unexpected Slack call %s", method)
			}
		})
		require.NoError(t, runResumeWithClient(t.Context(), CmdResume, []string{dir, "CROOT:" + parent}, testResumeStarter(api)))
		assert.Equal(t, []bool{true, true}, sessionFinishes(t, conn))
		var saved string
		require.NoError(t, conn.Get(&saved, `SELECT TXT FROM MESSAGE`))
		assert.Equal(t, root.Text, saved)
	})

	t.Run("old root, history, canvas and refresh", func(t *testing.T) {
		configureSearchResume(t)
		resumeFlags.SkipCompleteThreads = true
		resumeFlags.Refresh = true
		root := testSlackMessage(parent, parent, "old real parent", "FROOT")
		first := testSlackMessage(reply, parent, "in-window reply", "FREPLY")
		second := testSlackMessage(reply2, parent, "second reply", "FREPLYSECOND")
		lastTS := "1794801600.000012"
		last := testSlackMessage(lastTS, parent, "third reply", "FREPLYTHIRD")
		nohit := testSlackMessage(known, known, "known thread with no hit", "FHISTORY")
		nohit.LatestReply = "1794488401.000005"
		nohit.ReplyCount = 1
		nohitReply := testSlackMessage(nohit.LatestReply, known, "known reply", "")
		newRoot := testSlackMessage(known, known, "newly refreshed channel root", "")
		canvasRoot := testSlackMessage("1700000001.000002", "1700000001.000002", "canvas root", "")
		canvasRoot.SubType = structures.SubTypeDocumentCommentRoot
		canvasRoot.ReplyCount = 1
		canvasReply := testSlackMessage(reply, canvasRoot.Timestamp, "canvas reply", "")
		dir, conn := testArchive(t,
			chunk.Chunk{Type: chunk.CMessages, ChannelID: "COWNER", IsLast: true, Messages: []slack.Message{nohit}, Count: 1},
			chunk.Chunk{Type: chunk.CMessages, ChannelID: "CEMPTY", IsLast: true},
			chunk.Chunk{Type: chunk.CMessages, ChannelID: "CEXCLUDED", IsLast: true},
			chunk.Chunk{Type: chunk.CThreadMessages, ChannelID: "COWNER", IsLast: true, Parent: &nohit, ThreadTS: known,
				Messages: []slack.Message{nohit, nohitReply}, Count: 2},
		)

		var mu sync.Mutex
		calls := make(map[string]int)
		api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
			mu.Lock()
			defer mu.Unlock()
			channel := args.Get("channel")
			key := method + "/" + channel
			calls[key]++
			switch method {
			case "search.messages":
				assert.Equal(t, "is:thread after:2026-11-10 before:2026-11-21", args.Get("query"))
				switch args.Get("cursor") {
				case "*":
					return searchPage("next", 4, searchHit("COWNER", parent, first.Timestamp), searchHit("CEXCLUDED", parent, first.Timestamp)), nil
				case "next":
					return searchPage("", 4, searchHit("COWNER", parent, second.Timestamp), searchHit("CREFRESH", parent, first.Timestamp)), nil
				}
			case "conversations.list":
				return jsonObject{"ok": true, "channels": []slack.Channel{
					*structures.ChannelFromID("COWNER"), *structures.ChannelFromID("CREFRESH"), *structures.ChannelFromID("CEXCLUDED"),
				}}, nil
			case "conversations.info":
				if channel == "COWNER" {
					owner := structures.ChannelFromID(channel)
					owner.Properties = &slack.Properties{Canvas: slack.Canvas{FileId: "FCANVAS"}}
					return jsonObject{"ok": true, "channel": owner}, nil
				}
			case "conversations.history":
				switch channel {
				case "COWNER":
					knownTS, err := structures.ParseCanonicalSlackTS(known)
					assert.NoError(t, err)
					assert.Equal(t, structures.FormatSlackTS(knownTS.Add(-7*24*time.Hour)), args.Get("oldest"))
					return historyPage("", nohit), nil
				case "CEMPTY":
					assert.Equal(t, structures.FormatSlackTS(time.Time(cfg.Oldest)), args.Get("oldest"))
					return historyPage(""), nil
				case "CREFRESH":
					return historyPage("", newRoot), nil
				}
			case "files.info":
				assert.Equal(t, "FCANVAS", args.Get("file"))
				return jsonObject{"ok": true, "file": &slack.File{ID: "FCANVAS", Mode: "space"}}, nil
			case "conversations.replies":
				switch channel {
				case "COWNER":
					assert.Equal(t, parent, args.Get("ts"), "known no-hit threads must not be polled")
					assert.Equal(t, "1700000000.000000", args.Get("oldest"))
					assert.Equal(t, structures.FormatSlackTS(time.Time(cfg.Latest)), args.Get("latest"))
					assert.Equal(t, "0", args.Get("inclusive"))
					switch args.Get("cursor") {
					case "":
						return historyPage("root"), nil
					case "root":
						return historyPage("next", root, first), nil
					case "next":
						return historyPage("second"), nil
					case "second":
						return historyPage("third", second), nil
					case "third":
						otherParent := root
						otherParent.Text = "repeated parent metadata"
						return historyPage("last", otherParent, last), nil
					case "last":
						return historyPage(""), nil
					}
				case "CCANVAS":
					assert.Equal(t, canvasRoot.Timestamp, args.Get("ts"))
					return historyPage("", canvasRoot, canvasReply), nil
				case "CREFRESH":
					assert.Equal(t, newRoot.Timestamp, args.Get("ts"))
					return historyPage("", newRoot), nil
				}
			}
			if payload := commonResumeMethod(method, args); payload != nil {
				return payload, nil
			}
			return nil, fmt.Errorf("unexpected Slack call %s for %s", method, channel)
		})
		err := runResumeWithClient(t.Context(), CmdResume, []string{dir, "^CEXCLUDED"},
			testResumeStarter(&resumeCanvasClient{Slack: api, roots: []slack.Message{canvasRoot}}))
		require.NoError(t, err)
		assert.Equal(t, 2, calls["search.messages/"])
		assert.Equal(t, 6, calls["conversations.replies/COWNER"])
		assert.Equal(t, 1, calls["conversations.replies/CREFRESH"])
		assert.Equal(t, 1, calls["conversations.replies/CCANVAS"])
		assert.Zero(t, calls["conversations.history/CEXCLUDED"])
		assert.Equal(t, []bool{true, true}, sessionFinishes(t, conn))

		var saved []string
		err = conn.Select(&saved, `
SELECT DISTINCT M.TXT FROM MESSAGE M JOIN CHUNK C ON C.ID = M.CHUNK_ID
WHERE C.SESSION_ID = 2 AND C.TYPE_ID = ? AND C.CHANNEL_ID = 'COWNER' ORDER BY M.TXT`, chunk.CThreadMessages)
		require.NoError(t, err)
		assert.Equal(t, []string{first.Text, root.Text, second.Text, last.Text}, saved)
		var finals []bool
		require.NoError(t, conn.Select(&finals, `SELECT FINAL FROM CHUNK WHERE SESSION_ID = 2 AND TYPE_ID = ? AND CHANNEL_ID = 'COWNER' ORDER BY ID`, chunk.CThreadMessages))
		assert.Equal(t, []bool{false, false, false, true}, finals)
		var finalMessages []string
		require.NoError(t, conn.Select(&finalMessages, `
SELECT M.TXT FROM MESSAGE M JOIN CHUNK C ON C.ID = M.CHUNK_ID
WHERE C.SESSION_ID = 2 AND C.TYPE_ID = ? AND C.CHANNEL_ID = 'COWNER' AND C.FINAL`, chunk.CThreadMessages))
		assert.Equal(t, []string{root.Text}, finalMessages)
		var files []string
		require.NoError(t, conn.Select(&files, `SELECT F.ID FROM FILE F JOIN CHUNK C ON C.ID = F.CHUNK_ID WHERE C.SESSION_ID = 2 ORDER BY F.ID`))
		assert.Equal(t, []string{"FCANVAS", "FHISTORY", "FREPLY", "FREPLYSECOND", "FREPLYTHIRD", "FROOT"}, files)
		var types []int
		require.NoError(t, conn.Select(&types, `SELECT DISTINCT TYPE_ID FROM CHUNK WHERE SESSION_ID = 2`))
		assert.Contains(t, types, int(chunk.CCanvasMessages))
		assert.Contains(t, types, int(chunk.CCanvasThreadMessages))
		assert.NotContains(t, types, int(chunk.CSearchMessages))
	})

	t.Run("second search child failure happens before archive session", func(t *testing.T) {
		configureSearchResume(t)
		dir, conn := testArchive(t, chunk.Chunk{Type: chunk.CMessages, ChannelID: "COWNER", IsLast: true})
		const full = "is:thread after:2026-11-10 before:2026-11-21"
		const left = "is:thread after:2026-11-10 before:2026-11-17"
		const right = "is:thread after:2026-11-13 before:2026-11-21"
		var queries []string
		api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
			switch method {
			case "auth.test":
				return commonResumeMethod(method, args), nil
			case "search.messages":
				query := args.Get("query")
				queries = append(queries, query)
				assert.Equal(t, "*", args.Get("cursor"))
				switch query {
				case full:
					return searchPage("do-not-follow", 10_000), nil
				case left:
					return searchPage("", 1, searchHit("COWNER", parent, reply)), nil
				case right:
					return jsonObject{"ok": false, "error": "invalid_cursor"}, nil
				}
			}
			return nil, fmt.Errorf("unexpected Slack call %s", method)
		})
		err := runResumeWithClient(t.Context(), CmdResume, []string{dir}, testResumeStarter(api))
		require.ErrorContains(t, err, "invalid_cursor")
		assert.Equal(t, []string{full, left, right}, queries)
		assert.Equal(t, []bool{true}, sessionFinishes(t, conn))
	})

	for _, tc := range []struct {
		name    string
		handler func(url.Values) jsonObject
		want    string
	}{
		{"thread not found", func(url.Values) jsonObject { return jsonObject{"ok": false, "error": "thread_not_found"} }, "thread_not_found"},
		{"channel not found", func(url.Values) jsonObject { return jsonObject{"ok": false, "error": "channel_not_found"} }, "channel_not_found"},
		{"not in channel", func(url.Values) jsonObject { return jsonObject{"ok": false, "error": "not_in_channel"} }, "not_in_channel"},
		{"wrong parent", func(url.Values) jsonObject {
			return historyPage("", testSlackMessage(reply, parent, "reply instead of parent", ""))
		}, "does not begin with parent"},
		{"missing replies cursor on an empty page", func(url.Values) jsonObject {
			page := historyPage("")
			page["has_more"] = true
			return page
		}, "without a next cursor"},
		{"repeated replies cursor on an empty page", func(url.Values) jsonObject {
			return historyPage("next")
		}, "repeated replies cursor"},
		{"empty replies without a parent", func(args url.Values) jsonObject {
			if args.Get("cursor") == "" {
				return historyPage("next")
			}
			return historyPage("")
		}, "ended without parent"},
		{"malformed reply", func(url.Values) jsonObject {
			return historyPage("", testSlackMessage(parent, parent, "real root", ""), testSlackMessage(reply, known, "wrong thread", ""))
		}, "invalid reply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configureSearchResume(t)
			dir, conn := testArchive(t, chunk.Chunk{Type: chunk.CMessages, ChannelID: "COWNER", IsLast: true})
			api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
				switch method {
				case "search.messages":
					return searchPage("", 1, searchHit("COWNER", parent, reply)), nil
				case "conversations.history":
					return historyPage(""), nil
				case "conversations.replies":
					return tc.handler(args), nil
				default:
					return commonResumeMethod(method, args), nil
				}
			})
			err := runResumeWithClient(t.Context(), CmdResume, []string{dir}, testResumeStarter(api))
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, []bool{true, false}, sessionFinishes(t, conn))
			var empty int
			require.NoError(t, conn.Get(&empty, `SELECT COUNT(*) FROM MESSAGE WHERE TXT = ''`))
			assert.Zero(t, empty, "strict search roots must never store a placeholder")
		})
	}

	t.Run("failed thread fetch and same-window replay of partial history", func(t *testing.T) {
		configureSearchResume(t)
		resumeFlags.Lookback = new(extDuration)
		old := testSlackMessage("1794488400.000000", "", "completed old history", "")
		backlog := testSlackMessage("1794574800.000000", "", "older missing backlog", "")
		newest := testSlackMessage("1794801600.000000", "", "partial newest page", "")
		root := testSlackMessage(parent, parent, "canonical old root", "")
		recoveredReply := testSlackMessage(reply, parent, "canonical recent reply", "")
		dir, conn := testArchive(t, chunk.Chunk{Type: chunk.CMessages, ChannelID: "COWNER", IsLast: true, Messages: []slack.Message{old}, Count: 1})

		replay := false
		nextHistoryPage := make(chan struct{})
		api := testResumeClientWithContext(func(ctx context.Context, method string, args url.Values) (jsonObject, error) {
			switch method {
			case "search.messages":
				return searchPage("", 1, searchHit("COWNER", parent, reply)), nil
			case "conversations.history":
				assert.Equal(t, old.Timestamp, args.Get("oldest"), "unfinished newest page must not advance the lower bound")
				if args.Get("cursor") == "" {
					return historyPage("older", newest), nil
				}
				if replay {
					return historyPage("", backlog), nil
				}
				// Reaching the second request means the first history page was
				// recorded. Leave this channel incomplete until the thread fails.
				close(nextHistoryPage)
				<-ctx.Done()
				return nil, ctx.Err()
			case "conversations.replies":
				assert.Equal(t, root.Timestamp, args.Get("ts"))
				if replay {
					return historyPage("", root, recoveredReply), nil
				}
				select {
				case <-nextHistoryPage:
					return jsonObject{"ok": false, "error": "thread_not_found"}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			default:
				return commonResumeMethod(method, args), nil
			}
		})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		err := runResumeWithClient(ctx, CmdResume, []string{dir}, testResumeStarter(api))
		require.ErrorContains(t, err, "thread_not_found")
		assert.Equal(t, []bool{true, false}, sessionFinishes(t, conn))
		var written []string
		require.NoError(t, conn.Select(&written, "SELECT DISTINCT TXT FROM MESSAGE ORDER BY TXT"))
		assert.Equal(t, []string{old.Text, newest.Text}, written)

		replay = true
		require.NoError(t, runResumeWithClient(t.Context(), CmdResume, []string{dir}, testResumeStarter(api)))
		assert.Equal(t, []bool{true, false, true}, sessionFinishes(t, conn))
		written = nil
		require.NoError(t, conn.Select(&written, `
SELECT DISTINCT M.TXT FROM MESSAGE M JOIN CHUNK C ON C.ID = M.CHUNK_ID
WHERE C.TYPE_ID = ? ORDER BY M.TXT`, chunk.CMessages))
		assert.Equal(t, []string{old.Text, backlog.Text, newest.Text}, written)
		written = nil
		require.NoError(t, conn.Select(&written, `
SELECT M.TXT FROM MESSAGE M JOIN CHUNK C ON C.ID = M.CHUNK_ID
WHERE C.TYPE_ID = ? AND C.SESSION_ID = 3 ORDER BY M.TXT`, chunk.CThreadMessages))
		assert.Equal(t, []string{root.Text, recoveredReply.Text}, written)
	})

	t.Run("excludes-only resume does not call Slack", func(t *testing.T) {
		configureSearchResume(t)
		cfg.Oldest = cfg.TimeValue(time.Time{})
		dir, conn := testArchive(t, chunk.Chunk{Type: chunk.CMessages, ChannelID: "COWNER", IsLast: true})
		api := testResumeClient(func(method string, _ url.Values) (jsonObject, error) {
			return nil, fmt.Errorf("unexpected Slack call %s", method)
		})
		require.NoError(t, runResumeWithClient(t.Context(), CmdResume, []string{dir, "^COWNER"}, testResumeStarter(api)))
		assert.Equal(t, []bool{true}, sessionFinishes(t, conn))
	})

	t.Run("flag off still fetches history threads", func(t *testing.T) {
		configureSearchResume(t)
		resumeFlags.SearchThread = false
		cfg.Oldest = cfg.TimeValue(time.Time{})
		dir, conn := testArchive(t, chunk.Chunk{Type: chunk.CMessages, ChannelID: "COWNER", IsLast: true,
			Messages: []slack.Message{testSlackMessage(parent, "", "old history", "")}, Count: 1})
		root := testSlackMessage(known, known, "ordinary history root", "")
		api := testResumeClient(func(method string, args url.Values) (jsonObject, error) {
			switch method {
			case "search.messages":
				return nil, fmt.Errorf("unexpected search in stock resume")
			case "conversations.history":
				return historyPage("", root), nil
			case "conversations.replies":
				assert.Equal(t, root.Timestamp, args.Get("ts"))
				assert.Empty(t, args.Get("oldest"))
				return historyPage("", root), nil
			default:
				return commonResumeMethod(method, args), nil
			}
		})
		require.NoError(t, runResumeWithClient(t.Context(), CmdResume, []string{dir}, testResumeStarter(api)))
		assert.Equal(t, []bool{true, true}, sessionFinishes(t, conn))
		var found string
		require.NoError(t, conn.Get(&found, "SELECT M.TXT FROM MESSAGE M JOIN CHUNK C ON C.ID = M.CHUNK_ID WHERE C.TYPE_ID = ?", chunk.CThreadMessages))
		assert.Equal(t, root.Text, found)
	})
}

func sessionFinishes(t *testing.T, conn *sqlx.DB) []bool {
	t.Helper()
	var finished []bool
	require.NoError(t, conn.Select(&finished, "SELECT FINISHED FROM SESSION ORDER BY ID"))
	return finished
}

func historyPage(next string, messages ...slack.Message) jsonObject {
	return jsonObject{
		"ok": true, "messages": messages, "has_more": next != "",
		"response_metadata": jsonObject{"next_cursor": next},
	}
}
