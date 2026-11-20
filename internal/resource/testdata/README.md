# Resource fixtures

The Go tests use `upstream_oracle_corpus.json` and `upstream_oracle.json` to
check YAML frontmatter handling and the order and provenance of prompt templates.

## Reproducing the recorded results (optional)

The snapshot was captured from `parseFrontmatter` and `loadPromptTemplates`
in the pi revision recorded in the corpus. From that checkout, run:

```sh
PI_UPSTREAM_ROOT=/path/to/pi \
/path/to/pi/node_modules/.bin/tsx /path/to/pi-go/internal/resource/testdata/generate_upstream_oracle.ts
```

The command prints JSON for review; it does not overwrite the checked-in oracle.
