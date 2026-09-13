#!/usr/bin/env python3
"""Refuse a workflow file GitHub would refuse, or a cache filter buildkit would ignore.

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

Second check, same trigger: every `no-cache-filters` value on a
docker/build-push-action step must name a stage (`FROM … AS <name>`) of the
Dockerfile that step builds. buildkit matches the filter against stage names
with a plain case-insensitive compare and treats a name that matches nothing
as "filter nothing" — no warning, no error (frontend/dockerui/config.go,
IsNoCache). publish-container.yml relies on that filter to rebuild the shipped
stage from the current Alpine index instead of replaying a cached `apk
upgrade` layer; rename the stage in the Dockerfile and the filter silently
stops applying, the cache replays yesterday's package set, and the release
goes green with the old OpenSSL. This turns that rename into a guard failure.

TWIN: byte-identical in vidra-core, vidra-search and vidra-user. Change it in
all three or in none.
"""
import pathlib
import re
import sys

import yaml

BUILD_PUSH_ACTION = "docker/build-push-action@"

# `FROM [--flag=value ...] <image> AS <name>`. The Dockerfile parser accepts
# `AS` in any case and lowercases the name (instructions/parse.go,
# parseBuildStageName), so the comparison below lowercases both sides.
STAGE_RE = re.compile(
    r"^[ \t]*FROM[ \t]+(?:--\S+[ \t]+)*\S+[ \t]+AS[ \t]+(\S+)",
    re.IGNORECASE | re.MULTILINE)


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


def _repo_root(path):
    """The checkout a workflow belongs to: <root>/.github/workflows/<file>.

    Resolved from the workflow's own location rather than the working
    directory, so a copy of a workflow under some other root is checked
    against THAT root's Dockerfile.
    """
    workflow = pathlib.Path(path).resolve()
    if workflow.parent.name == "workflows" and workflow.parent.parent.name == ".github":
        return workflow.parent.parent.parent
    return pathlib.Path.cwd()


def _filters(value):
    """build-push-action splits `no-cache-filters` on newlines AND commas."""
    return [item.strip() for item in re.split(r"[\n,]", str(value)) if item.strip()]


def _stages(dockerfile):
    text = dockerfile.read_text(encoding="utf-8")
    text = re.sub(r"\\\r?\n", " ", text)  # join backslash continuations
    return {match.group(1).lower() for match in STAGE_RE.finditer(text)}


def _cache_filter_problems(path, workflow):
    """Every no-cache-filters entry must name a stage of the Dockerfile it builds."""
    problems = []
    verified = 0
    jobs = workflow.get("jobs") if isinstance(workflow, dict) else None
    for job_id, job in (jobs or {}).items():
        steps = job.get("steps") if isinstance(job, dict) else None
        for step in steps or []:
            if not isinstance(step, dict):
                continue
            if not str(step.get("uses", "")).startswith(BUILD_PUSH_ACTION):
                continue
            inputs = step.get("with") or {}
            filters = _filters(inputs.get("no-cache-filters", ""))
            if not filters:
                continue
            where = f"job {job_id!r}, step {step.get('name') or step.get('id') or '?'!r}"
            # build-push-action resolves `file` against the workspace and
            # defaults it to <context>/Dockerfile.
            context = str(inputs.get("context") or ".")
            dockerfile = str(inputs.get("file") or pathlib.PurePosixPath(context) / "Dockerfile")
            if "${{" in dockerfile or "://" in context:
                problems.append(
                    f"{where}: cannot resolve the Dockerfile behind no-cache-filters "
                    f"({dockerfile!r}); use a literal path so its stage names can be checked")
                continue
            target = _repo_root(path) / dockerfile
            if not target.is_file():
                problems.append(
                    f"{where}: no-cache-filters {filters} but {dockerfile!r} does not exist")
                continue
            stages = _stages(target)
            for name in filters:
                if name.lower() in stages:
                    verified += 1
                    continue
                problems.append(
                    f"{where}: no-cache-filters names stage {name!r}, which {dockerfile} "
                    f"does not define (stages: {', '.join(sorted(stages)) or 'none'}); "
                    "buildkit would silently filter nothing and the cache would replay "
                    "the shipped stage")
    return problems, verified


def main(paths):
    rejected = 0
    verified = 0
    for path in paths:
        try:
            with open(path, encoding="utf-8") as handle:
                workflow = yaml.load(handle, Loader=Strict)
        except yaml.YAMLError as error:
            print(f"::error file={path}::GitHub would reject this workflow file: {error}")
            rejected += 1
            continue
        problems, count = _cache_filter_problems(path, workflow)
        verified += count
        for problem in problems:
            print(f"::error file={path}::{problem}")
        if problems:
            rejected += 1
    print(f"workflow yaml: {len(paths)} file(s) checked, {rejected} rejected; "
          f"no-cache-filters: {verified} stage reference(s) verified")
    return 1 if rejected else 0


if __name__ == "__main__":
    files = sys.argv[1:] or sorted(
        str(p) for p in pathlib.Path(".github/workflows").glob("*.y*ml"))
    if not files:
        print("::error::no workflow files found under .github/workflows")
        sys.exit(1)
    sys.exit(main(files))
