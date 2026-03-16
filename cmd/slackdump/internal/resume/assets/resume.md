# Resume command

Resume allows to continue the archive where you left off.

It may be useful in the following situations:
- Slackdump crashed or was interrupted;
- You want to add data to an existing archive.

Please note that archive must be in database format (default for "archive"
command).

## How it works

Resume loads the latest timestamps for each channel from the existing archive,
then fetches only new messages since those timestamps. By default, it
processes channels only (not threads).

### Thread handling (`-threads` flag)

When the `-threads` flag is specified, resume uses an optimized two-part
strategy to discover threads:

1. **New threads from channel messages**: Threads are discovered from messages
   in the lookback window (new conversations started recently).
2. **Active threads from database**: The database is queried for threads with
   `LATEST_REPLY` timestamps within the lookback window. This catches ongoing
   conversations in old threads that received new replies.

This approach balances efficiency with completeness - it avoids scanning all
historical threads while still catching active conversations.

**Limitations**: Resume may miss new threads that were started in channel
messages older than the lookback window. For example, if a user starts a
new thread reply on a 30-day-old message, but your lookback window is
7 days, that new thread activity will not be captured (since the parent
message falls outside the lookback window and the thread itself has no
prior activity to trigger the active thread query).

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

__NOTE__: Resume is in beta and may not work as expected. Please report any
issues on GitHub.

