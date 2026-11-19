# Resume command

Resume allows to continue the archive where you left off.

It may be useful in the following situations:
- Slackdump crashed or was interrupted;
- You want to add data to an existing archive.

Please note that archive must be in database format (default for "archive"
command).

### Resuming Export, Chunk, or Dump formats.
If you want to resume a Slack Export, Chunk, or dump formats, follow these
steps:

1. Convert it to database format first:
    
   ```plaintext
   slackdump convert -format database <your-export .zip or directory>
   ```

   This will create a new directory with Slackdump database, i.e.
   `slackdump_20241231_150405`.
2. Resume the archive:

   ```plaintext
   slackdump resume slackdump_20241231_150405
   ```

   This will continue the archive where you left off.
3. When Slackdump finishes, the archive will be updated with the
   latest data.  Convert it back to the desired format:
   ```bash
   slackdump convert -format <your-format> slackdump_20241231_150405
   ```

### Discovering new channels.

Use `-refresh` to discover additional channels while resuming an existing
archive. Add `-member-only` to skip newly discovered public channels that you
have not joined:

```plaintext
slackdump resume -refresh -member-only <archive>
```

The membership filter applies to newly discovered channels. Channels already
in the resume list are still processed, even if you are no longer a member.

### Discovering thread activity with Slack search.

```plaintext
slackdump resume --search-thread <archive>
slackdump resume --search-thread -lookback p28d <archive>
```

`--search-thread` replaces ordinary thread polling for archived and explicitly
included channels with discovery through Slack's `is:thread` search. It takes
precedence over `-threads`. The UTC start is inclusive. Set it with `-time-from`
or let Slackdump use the earliest channel resume start in the search scope.
For an archived channel, the resume start is the timestamp of its latest
message from a completed history fetch, minus `-lookback` (seven days by
default). Specifying a channel on the command line replaces its archived resume
start with the date you supply, if any. Channels without a date share the search
window. If the scope has channels but none has a date, supply `-time-from`.
The UTC `-time-to` is exclusive and defaults to the time Slackdump started.

Slackdump searches the full window and splits at UTC midnight boundaries when
the first page reports at least 10,000 matches or 100 pages. Reaching page 100
also triggers a split as a precaution for the hypothetical case where Slack
clips results and understates the first-page counts. If a capped window has
no interior UTC midnight, discovery fails. Slackdump filters hits by their
exact timestamp. A reply in the window can lead to a parent older than the
window.
Discovered threads are fetched from the parent through the exclusive end.

Channel history and canvas discussions still resume. Channel exclusions and
`-skip-stale-channels` limit discovery. An explicit channel can override the
stale filter. For channel history, `-time-from` serves as a fallback for
channels without their own start. Threads specified by link retain their usual
bounds and `-skip-complete-threads` policy. Search-discovered roots bypass
`-skip-complete-threads` and `-skip-stale-threads`. Channels added by
`-refresh` outside the initial search scope use ordinary history-based thread
discovery. This includes channels that `-refresh` adds back after the stale
channel filter skipped them.

Choose a start with overlap to account for indexing delays. Use the same explicit
bounds to replay a failed run.

```plaintext
slackdump resume --search-thread -time-from 2026-11-12T00:00:00 \
  -time-to 2026-11-19T00:00:00 <archive>
```

Search discovery runs before the archive session starts. A failed fetch for a
search-discovered thread leaves the session unfinished. Slackdump stores no
search checkpoint. Search coverage depends on Slack indexing, visibility, and
retention and may miss edits or deletions.

### Deduplicating resume overlap.

Resume uses a lookback window to avoid missing edits, replies, and other
late-arriving changes. This can re-fetch unchanged entities that are already in
the archive. Use `-dedupe` to remove identical duplicate messages, users,
channels, channel users, files, and now-empty chunks after a successful resume:

```plaintext
slackdump resume -dedupe slackdump_20241231_150405
```

Deduplication runs only after resume finishes successfully. To preview or run
the same cleanup manually later, use `slackdump tools dedupe`.

### Skipping complete threads.

When `-threads` is enabled, resume normally re-fetches every known thread in
the lookback window. Use `-skip-complete-threads` to skip a thread when the
database already contains the parent message plus the number of replies
reported by Slack:

```plaintext
slackdump resume -threads -skip-complete-threads <archive>
```

This is faster for append-only archives, but it will not detect edits or
deletions inside a thread that already appears complete. Direct thread resume
items are still kept; the complete-thread check runs after the first successful
`conversations.replies` page so Slack's current `reply_count` is available.

### Skipping stale entities.

Long-lived archives accumulate channels and threads that have not received any
new activity for weeks or months. Resuming such archives spends most of its
wall-clock time fetching the first page of `conversations.replies` for
thread parents that will never get another reply, which also burns
rate-limit budget on `conversations.replies` and is the most common cause of
Slack 429 retries during resume.

`-skip-stale-threads` and `-skip-stale-channels` filter dormant entities out
of the entity list **before** any API call fires. Both flags accept an ISO
8601 duration (e.g. `p21d`, `p7d`, `p2w`) and default to disabled when not
set:

```plaintext
# Skip threads whose latest reply is older than 21 days; channels untouched.
slackdump resume -threads -skip-stale-threads p21d <archive>

# Skip dormant channels in addition. Pair with a periodic full-sweep run
# (e.g. a daily cron without the skip-stale flags) so dormant channels
# are still revisited and resurrections are surfaced.
slackdump resume -threads -skip-stale-threads p21d -skip-stale-channels p21d <archive>
```

The two flags are independent. Skipping stale **channels** is the more
aggressive trade because brand-new top-level messages in a skipped channel
will not be picked up until that channel is included again — pair with a
periodic full sweep. Skipping stale **threads** is conservative: dormant
thread parents are very unlikely to receive new replies, and a full sweep
will still catch them.
If stale filters skip every resume candidate, resume exits successfully as a
no-op before setting up a Slack API session.

__NOTE__: Resume is in beta and may not work as expected. Please report any
issues on GitHub.
