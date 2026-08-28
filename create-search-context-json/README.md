# Create a search context from a repository list

`create_search_context.py` reads a `repos.txt` file, verifies every repository through Sourcegraph's GraphQL API, resolves each repository's default branch, and creates a search context pinned to those branch names.

Missing repositories and repositories without a default branch are reported to stderr and omitted from the context.

## Usage

Set the same environment variables used by `src-cli`:

```sh
export SRC_ENDPOINT=https://sourcegraph.example.com
export SRC_ACCESS_TOKEN=sgp_your_token
```

For an instance using a private CA, set `SSL_CERT_FILE` or pass `--ca-bundle PATH`.

Create a private context owned by the authenticated user:

```sh
./create_search_context.py --name platform --repos-file repos.txt
```

Inspect the resolved search-context JSON without creating anything:

```sh
./create_search_context.py --name platform --repos-file repos.txt --dry-run
```

Use `--public` to make the context visible to other users. Use `--global` to create an instance-level context; this requires the appropriate site permission.

The repository file uses the same format as `../repo-population-insights/repos.txt`: one repository name per line, with blank lines and lines beginning with `#` ignored. Repository filters copied from a search query are also accepted.

```text
# Application repositories
github.com/acme/frontend
repo:^github\.com/acme/backend$
```

Run `./create_search_context.py --help` for all options.
