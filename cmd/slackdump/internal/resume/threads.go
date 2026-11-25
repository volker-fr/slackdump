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

// Live Slack search trials, 2026-11-19.
//
// We searched the same 90 days in three workspaces, once as a full
// window and again in seven-day slices, with six days in the last slice.
// The combined slices returned the same messages as the full search.
//
// We counted each archived thread once if its parent or a reply was
// posted during the window. The older parents were posted before the
// window and discovered through replies posted within it.
//
// Reply counts cover archived replies still present in Slack.
//
//   Workspace A found all 172 threads, including 13 with older parents.
//   Search matched all 680 of 680 archived replies.
//
//   Workspace B found all 114 threads, including 4 with older parents.
//   Search matched all 653 archived replies.
//
//   Workspace C found all 501 threads, including 38 with older parents.
//   Search matched all 2,410 archived replies.
//
// A separate direct fetch retrieved all 13 older parents in workspace A.

package resume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rusq/slack"

	"github.com/rusq/slackdump/v4/cmd/slackdump/internal/cfg"
	"github.com/rusq/slackdump/v4/internal/structures"
	"github.com/rusq/slackdump/v4/source"
	"github.com/rusq/slackdump/v4/stream"
)

func registerThreadFlags() {
	CmdResume.Flag.BoolVar(&resumeFlags.IncludeThreads, "threads", false, "include threads (slow, and flaky business)")
	CmdResume.Flag.BoolVar(&resumeFlags.SearchThread, "search-thread", false, "discover ordinary threads with Slack search instead of polling known threads")
	CmdResume.Flag.BoolVar(&resumeFlags.SkipCompleteThreads, "skip-complete-threads", false, "skip threads where DB already has all replies (faster, but won't detect edits/deletes)")
	CmdResume.Flag.Var(resumeFlags.SkipStaleThreads, "skip-stale-threads", "skip thread entities whose latest reply is older than this `duration` (default: disabled)")
}

type latestResult struct {
	list          *structures.EntityList
	hasSourceData bool
	skippedStale  int
}

func latest(ctx context.Context, src source.Resumer, includeThreads bool, skipCompleteThreads bool, lookBack time.Duration, threadCutoff, channelCutoff *time.Time, other *structures.EntityList) (latestResult, error) {
	if lookBack > 0 {
		lookBack = -lookBack
	}
	latest, err := src.Latest(ctx)
	if err != nil {
		return latestResult{}, fmt.Errorf("error loading latest timestamps: %w", err)
	}
	result := latestResult{
		list:          &structures.EntityList{},
		hasSourceData: len(latest) > 0,
	}
	if len(latest) == 0 && (other == nil || other.IsEmpty()) {
		return result, nil
	}

	if cfg.Verbose {
		strlatest(latest)
	}

	ei := make([]structures.EntityItem, 0, len(latest))
	for sl, ts := range latest {
		if sl.IsThread() && !includeThreads {
			continue
		}
		if sl.IsThread() && threadCutoff != nil && ts.Before(*threadCutoff) {
			result.skippedStale++
			continue
		}
		if !sl.IsThread() && channelCutoff != nil && ts.Before(*channelCutoff) {
			result.skippedStale++
			continue
		}
		item := structures.EntityItem{
			Id:      sl.String(),
			Oldest:  ts.Add(lookBack),
			Latest:  time.Time(cfg.Latest),
			Include: true,
		}
		ei = append(ei, item)
		debugprint(fmt.Sprintf("%s: %d->%d", item.Id, ts.UTC().UnixMicro(), item.Oldest.UnixMicro()))
	}
	el := structures.NewEntityListFromItems(ei...)
	el.Overlay(other)
	result.list = el

	return result, nil
}

func debugprint(a ...any) {
	if cfg.Verbose {
		fmt.Println(a...)
	}
}

func strlatest(l map[structures.SlackLink]time.Time) string {
	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 0, 1, ' ', 0)
	fmt.Fprintln(tw, "Group ID\tLatest")
	for gid, ts := range l {
		fmt.Fprintf(tw, "%s\t%s\n", gid, ts.Format("2006-01-02 15:04:05 MST"))
	}
	if err := tw.Flush(); err != nil {
		slog.Error("flushing went wrong", "error", err)
	}
	return buf.String()
}

type searchThreadWindow struct {
	from, to time.Time
}

func newSearchThreadWindow(from, to time.Time) (searchThreadWindow, error) {
	if from.IsZero() {
		return searchThreadWindow{}, errors.New("cannot infer a search start from the selected channels; specify -time-from")
	}
	if !from.Before(to) {
		return searchThreadWindow{}, errors.New("-search-thread start must be earlier than -time-to")
	}
	// Slack timestamps have microsecond precision. Rounding an exclusive
	// bound upward preserves all Slack timestamps before the requested end.
	end := to.UTC().Truncate(time.Microsecond)
	if end.Before(to) {
		end = end.Add(time.Microsecond)
	}
	return searchThreadWindow{from: from.UTC(), to: end}, nil
}

func searchThreadChannels(history map[string]time.Time, lookback time.Duration, cutoff *time.Time, explicit *structures.EntityList) (latestResult, map[string]struct{}, time.Time) {
	if lookback > 0 {
		lookback = -lookback
	}
	result := latestResult{hasSourceData: len(history) > 0}
	items := make([]structures.EntityItem, 0, len(history))
	for channelID, timestamp := range history {
		var oldest time.Time
		if !timestamp.IsZero() {
			if cutoff != nil && timestamp.Before(*cutoff) {
				result.skippedStale++
				continue
			}
			oldest = timestamp.Add(lookback)
		}
		items = append(items, structures.EntityItem{
			Id: channelID, Include: true, Oldest: oldest, Latest: time.Time(cfg.Latest),
		})
	}
	result.list = structures.NewEntityListFromItems(items...)
	result.list.Overlay(explicit)

	channels := make(map[string]struct{})
	var earliest time.Time
	for _, item := range result.list.Index() {
		if !item.Include || strings.Contains(item.Id, structures.LinkSep) {
			continue
		}
		channels[item.Id] = struct{}{}
		if !item.Oldest.IsZero() && (earliest.IsZero() || item.Oldest.Before(earliest)) {
			earliest = item.Oldest
		}
	}
	return result, channels, earliest
}

func decideSearchResume(result latestResult, refresh bool) resumeDecision {
	if result.list.HasIncludes() {
		return resumeDecisionContinue
	}
	if !result.hasSourceData && !result.list.HasExcludes() {
		return resumeDecisionInvalidArchive
	}
	if refresh {
		return resumeDecisionContinue
	}
	return resumeDecisionNoop
}

func (w searchThreadWindow) addThreads(ctx context.Context, s *stream.Stream, list *structures.EntityList, channels map[string]struct{}) (stream.Option, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roots := make(map[structures.SlackLink]struct{})
	if len(channels) == 0 {
		return stream.OptSearchThreadResume(channels, roots), nil
	}

	// Remember explicit roots and exclusions, including timestamp spellings
	// accepted by the direct-link parser that omit trailing fractional zeros.
	explicit := make(map[string]struct{})
	for id := range list.Index() {
		channel, ts, ok := strings.Cut(id, structures.LinkSep)
		if !ok {
			continue
		}
		if _, fraction, ok := strings.Cut(ts, "."); ok && len(fraction) < 6 {
			ts += strings.Repeat("0", 6-len(fraction))
		}
		explicit[channel+structures.LinkSep+ts] = struct{}{}
	}

	var items []structures.EntityItem
	skippedMalformed := 0
	const (
		day              = 24 * time.Hour
		searchMatchLimit = 10_000
		searchPageLimit  = 100
	)
	errSearchLimit := errors.New("search result limit reached")
	var search func(from, to time.Time) error
	search = func(from, to time.Time) error {
		firstDay, endDay := from.UTC().Truncate(day), to.UTC().Truncate(day)
		if endDay.Before(to) {
			endDay = endDay.AddDate(0, 0, 1)
		}
		query := fmt.Sprintf("is:thread after:%s before:%s",
			firstDay.AddDate(0, 0, -2).Format(time.DateOnly),
			endDay.AddDate(0, 0, 2).Format(time.DateOnly))
		pages, rawMatches, total, reportedPages := 0, 0, 0, 0
		cursors := map[string]struct{}{"*": {}}
		pending := make(map[structures.SlackLink]time.Time)
		malformed := 0
		err := s.SearchMessagePages(ctx, query, func(cursor string, page *slack.SearchMessages) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			pages++
			rawMatches += len(page.Matches)
			if pages == 1 {
				total = max(page.Total, page.Paging.Total, page.Pagination.TotalCount)
				reportedPages = max(page.Paging.Pages, page.Pagination.PageCount)
			}
			cfg.Log.DebugContext(ctx, "thread search page",
				"from", from, "to", to, "cursor", cursor, "next_cursor", page.NextCursor,
				"total", total, "matches", len(page.Matches), "raw_matches", rawMatches)
			if page.NextCursor != "" {
				if _, seen := cursors[page.NextCursor]; seen {
					return errors.New("repeated search cursor")
				}
				cursors[page.NextCursor] = struct{}{}
			}
			if pages == 1 && (total >= searchMatchLimit || reportedPages >= searchPageLimit) {
				return errSearchLimit
			}
			// The trials did not show clipping with understated first-page counts.
			// Stop at page 100 in case Slack does clip results in that situation.
			if pages == searchPageLimit {
				return errSearchLimit
			}
			for _, hit := range page.Matches {
				if _, ok := channels[hit.Channel.ID]; !ok {
					continue
				}
				hitTS, err := structures.ParseCanonicalSlackTS(hit.Timestamp)
				if err != nil {
					malformed++
					continue
				}
				if hitTS.Before(from) || !hitTS.Before(to) {
					continue
				}
				root, parentTS, ok := parseSearchThreadRoot(hit, hitTS)
				if !ok {
					malformed++
					continue
				}
				if _, exists := explicit[root.String()]; exists {
					continue
				}
				if _, exists := roots[root]; exists {
					continue
				}
				pending[root] = parentTS
			}
			if page.NextCursor == "" && rawMatches < total {
				// The reason is unclear. Slack might still count a deleted message in its search cache.
				cfg.Log.WarnContext(ctx, fmt.Sprintf(
					"Slack search returned fewer matches than reported (%d out of %d results missing). Missing hits can belong to threads already identified.",
					total-rawMatches, total), "from", from, "to", to, "total", total, "raw_matches", rawMatches)
			}
			return nil
		})
		if errors.Is(err, errSearchLimit) {
			// Splitting the covered days at an interior midnight narrows both date queries.
			days := (endDay.Unix() - firstDay.Unix()) / int64(day/time.Second)
			mid := firstDay.AddDate(0, 0, int(days/2))
			if !from.Before(mid) || !mid.Before(to) {
				return fmt.Errorf("thread search [%s, %s): %w after %d pages and %d matches (%d matches and %d pages reported); no interior UTC midnight",
					from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), err, pages, rawMatches, total, reportedPages)
			}
			cfg.Log.InfoContext(ctx, "splitting thread search window", "from", from, "to", to, "split", mid,
				"pages", pages, "reported_pages", reportedPages, "total", total, "raw_matches", rawMatches)
			if err := search(from, mid); err != nil {
				return err
			}
			return search(mid, to)
		}
		if err != nil {
			return fmt.Errorf("thread search [%s, %s): %w", from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), err)
		}
		for root, parentTS := range pending {
			roots[root] = struct{}{}
			items = append(items, structures.EntityItem{
				Id: root.String(), Include: true,
				Oldest: parentTS.Add(-time.Microsecond), Latest: w.to,
			})
		}
		skippedMalformed += malformed
		cfg.Log.InfoContext(ctx, "thread search window complete", "from", from, "to", to,
			"pages", pages, "total", total, "raw_matches", rawMatches)
		return nil
	}
	if err := search(w.from, w.to); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg.Log.InfoContext(ctx, "thread search discovery complete", "channels", len(channels),
		"roots", len(roots), "skipped_malformed", skippedMalformed)

	list.Overlay(structures.NewEntityListFromItems(items...))
	return stream.OptSearchThreadResume(channels, roots), nil
}

// parseSearchThreadRoot identifies the root from thread_ts. The permalink
// path names the matched message itself, which may be a reply.
func parseSearchThreadRoot(hit slack.SearchMessage, hitTS time.Time) (structures.SlackLink, time.Time, bool) {
	link, err := url.Parse(hit.Permalink)
	if err != nil || link.Scheme != "https" || link.Host == "" {
		return structures.SlackLink{}, time.Time{}, false
	}
	path := strings.Split(strings.TrimPrefix(link.Path, "/"), "/")
	if len(path) != 3 || path[0] != "archives" || path[1] != hit.Channel.ID ||
		path[2] != "p"+strings.Replace(hit.Timestamp, ".", "", 1) {
		return structures.SlackLink{}, time.Time{}, false
	}
	query, err := url.ParseQuery(link.RawQuery)
	if err != nil || len(query["thread_ts"]) != 1 {
		return structures.SlackLink{}, time.Time{}, false
	}
	if cid := query["cid"]; len(cid) > 0 && (len(cid) != 1 || cid[0] != hit.Channel.ID) {
		return structures.SlackLink{}, time.Time{}, false
	}
	root := structures.SlackLink{Channel: hit.Channel.ID, ThreadTS: query.Get("thread_ts")}
	parentTS, err := structures.ParseCanonicalSlackTS(root.ThreadTS)
	if err != nil || parentTS.UnixMicro() <= 0 || parentTS.After(hitTS) {
		return structures.SlackLink{}, time.Time{}, false
	}
	if _, err := structures.ParseLink(root.String()); err != nil {
		return structures.SlackLink{}, time.Time{}, false
	}
	return root, parentTS, true
}
