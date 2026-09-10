#!/usr/bin/env python3
"""Refuse a workflow file GitHub would refuse.

Every YAML library, PyYAML included, reads a mapping with a duplicated key and
silently keeps one of the values. GitHub's workflow parser rejects the file.
The run is still created — conclusion "failure", ZERO jobs — and because no
job ever starts, no check-run is produced, so a lane the required-checks
manifest lists as `?name` (required only if it ran) simply stops existing and
ci-required stays green.

That is not hypothetical. vidra-search's rollback-floor.yml carried two
top-level `env:` blocks from 2026-09-08 05:53 to 2026-09-10: every run failed
this way, through two release cuts, with ci-required green throughout, and the
one-release rollback floor for the search schema was unenforced the whole time.
`ruby -ryaml` and `python3 -c 'import yaml'` both said the file was fine.

So this loads every workflow with a mapping constructor that refuses a
repeated key at ANY depth — a job with two `env:` blocks or two `steps:` lists
breaks the same way. It runs in ci-guard, which triggers on workflow changes:
the moment a duplicate can be introduced.

TWIN: byte-identical in vidra-core, vidra-search and vidra-user. Change it in
all three or in none.
"""
import pathlib
import sys

import yaml


class Strict(yaml.SafeLoader):
    """SafeLoader that refuses duplicate mapping keys instead of picking one."""


def _mapping(loader, node, deep=False):
    seen = {}
    for key_node, _ in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in seen:
            raise yaml.constructor.ConstructorError(
                None, None,
                f"duplicate key {key!r} (first defined on line {seen[key]})",
                key_node.start_mark)
        seen[key] = key_node.start_mark.line + 1
    return yaml.SafeLoader.construct_mapping(loader, node, deep)


Strict.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping)


def main(paths):
    rejected = 0
    for path in paths:
        try:
            with open(path, encoding="utf-8") as handle:
                yaml.load(handle, Loader=Strict)
        except yaml.YAMLError as error:
            print(f"::error file={path}::GitHub would reject this workflow file: {error}")
            rejected += 1
    print(f"workflow yaml: {len(paths)} file(s) checked, {rejected} rejected")
    return 1 if rejected else 0


if __name__ == "__main__":
    files = sys.argv[1:] or sorted(
        str(p) for p in pathlib.Path(".github/workflows").glob("*.y*ml"))
    if not files:
        print("::error::no workflow files found under .github/workflows")
        sys.exit(1)
    sys.exit(main(files))
