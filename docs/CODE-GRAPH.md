# Code graph

Git tracks `graphify-out/graph.json`. Graphify 0.9.105 builds it without model calls.
The graph maps Go, JavaScript and Python code, tests and authored Markdown.

## Ownership

The main branch owns the graph. Work PRs do not change it, thus the graph causes no merge conflicts.

1. You merge a PR into main.
2. The `graphify` workflow rebuilds the graph from main.
3. If the graph changes, the workflow opens the `ci/graphify-refresh` PR. This PR changes only the graph.
4. The workflow sets squash auto-merge on this PR. After the required checks and the review, GitHub merges it.

Each push to main replaces the refresh PR content with a graph for the newest commit.
Between a merge and the refresh merge, the graph on main can be older than the code.

The `graph ownership` check stops a work PR that changes the graph.
On the refresh PR, the same check rebuilds the graph and compares it with the PR.

## Scope

Code extraction covers tracked Go in `cmd` and `internal`, and JavaScript in `clients`.
It also covers Python in `training` and tests in `tests`.
Markdown extraction covers `README.md`, `SECURITY.md`, `AGENTS.md` and documents in `docs`.

The graph does not include CI tools, dependencies, build output or generated documents.

## Queries

Install the pinned tools once with Python 3.12:

```sh
python3.12 -m venv ~/.local/share/gpu-workload-supervisor-graphify
~/.local/share/gpu-workload-supervisor-graphify/bin/python -m pip install --require-hashes -r .github/graphify/requirements.txt
```

Use focused queries:

```sh
~/.local/share/gpu-workload-supervisor-graphify/bin/graphify query 'How does the proxy start a workload?' --graph graphify-out/graph.json --budget 2000
```

Look at the source when the graph shows no edge or an unclear edge. A graph does not prove behavior.

## Local checks

Rebuild the graph and compare it with the committed file:

```sh
GRAPHIFY_BACKEND=none PYTHONHASHSEED=0 ~/.local/share/gpu-workload-supervisor-graphify/bin/python .github/graphify/snapshot.py --check
```

The check fails on a work branch after source changes. This result is correct. Do not commit the rebuilt graph.
