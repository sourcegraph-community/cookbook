# Create a capture-group insight from a repository list

`create_capture_group_insight.py` creates a Sourcegraph code insight scoped to an explicit list of repositories. Each value matched by the query's capturing group becomes a separate series.

The script can validate repository names, preview the generated series, and attach the insight to an existing dashboard.

## Usage

Set the same environment variables used by `src-cli`:

```sh
export SRC_ENDPOINT=https://sourcegraph.example.com
export SRC_ACCESS_TOKEN=sgp_your_token
```

For an instance using a private CA, set `SSL_CERT_FILE` or pass `--ca-bundle PATH`.

Preview and validate an insight before creating it:

```sh
./create_capture_group_insight.py \
  --repos-file repos.txt \
  --title "Go versions" \
  --query 'patterntype:regexp file:^go\.mod$ ^go (\d+\.\d+)' \
  --validate-repos \
  --preview \
  --dry-run
```

Remove `--dry-run` to create the insight. Use `--dashboard "Platform migration"` to attach it to an existing dashboard.

The query must contain a capturing group around the value used for series labels. Use non-capturing groups such as `(?:...)` for other grouping:

```sh
--query 'patterntype:regexp file:Dockerfile$ ^FROM (?:--platform=\S+ )?(\S+)'
```

Copy `repos.txt.example` to `repos.txt` and add one repository name per line. Blank lines and lines beginning with `#` are ignored. Repository filters copied from a search query are also accepted.

```text
# Application repositories
github.com/acme/frontend
repo:^github\.com/acme/backend$
```

Run `./create_capture_group_insight.py --help` for all options and additional query examples.
