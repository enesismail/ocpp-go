#!/usr/bin/env python3
"""Compare two directories of OCPP Part 3 JSON schemas and report every difference.

Usage:
    python3 ocpp-schema-delta.py <old-schema-dir> <new-schema-dir>

Written for the OCPP 2.0.1 vs OCPP 2.1 comparison, but generic over any two
flat directories of OCA Part 3 message schemas (one *.json file per message,
named FooRequest.json / FooResponse.json, shared types inlined per file under
"definitions").

The comparison applies, to every schema file present in both directories:

  - top level: the root "required" array (as a set), the root "properties"
    key set, and the root "type";
  - nested: the same three checks for every named object under "definitions",
    plus, per definition: the "enum" value set, and per property: "maxLength".
  - per file: the set of definition names itself (a deleted or added
    definition — e.g. an enum type removed in favour of a plain string — is
    reported, not silently skipped).

Findings are aggregated across files (Part 3 inlines shared types into every
file that uses them, so one type-level change appears in many files) and
printed grouped by definition, with the list of affected files.

The report ends with a SHA-256 digest per directory so two runs can confirm
they compared the same bundles: each file's SHA-256 is formatted as
"<hex>  <filename>", the lines are sorted, and the digest is the SHA-256 of
that sorted listing — the digest moves if any file changes, arrives, leaves,
or is renamed. Requires only the Python 3 standard library.
"""

import hashlib
import json
import sys
from collections import defaultdict
from pathlib import Path


def directory_digest(files):
    lines = []
    for f in files:
        h = hashlib.sha256(f.read_bytes()).hexdigest()
        lines.append(f"{h}  {f.name}\n")
    return hashlib.sha256("".join(sorted(lines)).encode()).hexdigest()


def message_name(stem):
    """Strip the Request/Response suffix; None marks an unpaired file."""
    for suffix in ("Request", "Response"):
        if stem.endswith(suffix):
            return stem[: -len(suffix)], suffix
    return None, None


def load(path):
    with open(path, encoding="utf-8-sig") as fh:
        return json.load(fh)


def as_set(value):
    return set(value) if isinstance(value, list) else set()


def walk_properties(definition):
    """Yield (property_name, property_schema) for a definition's properties."""
    props = definition.get("properties")
    if isinstance(props, dict):
        yield from props.items()


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    old_dir, new_dir = Path(sys.argv[1]), Path(sys.argv[2])
    old_files = sorted(old_dir.glob("*.json"))
    new_files = sorted(new_dir.glob("*.json"))
    old_names = {f.name for f in old_files}
    new_names = {f.name for f in new_files}

    # --- inventory -------------------------------------------------------
    def inventory(files):
        paired, unpaired = set(), []
        for f in files:
            name, _ = message_name(f.stem)
            if name is None:
                unpaired.append(f.name)
            else:
                paired.add(name)
        return paired, unpaired

    old_msgs, old_unpaired = inventory(old_files)
    new_msgs, new_unpaired = inventory(new_files)
    unpaired_msgs = {Path(n).stem for n in new_unpaired}
    print(f"old: {len(old_files)} schema files, {len(old_msgs)} paired message names, "
          f"{len(old_unpaired)} unpaired file(s) {old_unpaired or ''}")
    print(f"new: {len(new_files)} schema files, "
          f"{len(new_msgs) + len(unpaired_msgs)} message names "
          f"({len(new_msgs)} paired, {len(new_unpaired)} unpaired file(s) {new_unpaired or ''})")
    all_new_names = new_msgs | unpaired_msgs
    print(f"shared message names: {len(old_msgs & all_new_names)}; "
          f"added: {len(all_new_names - old_msgs)}; removed: {len(old_msgs - all_new_names)}")
    for name in sorted(all_new_names - old_msgs):
        print(f"  added message: {name}")
    for name in sorted(old_msgs - all_new_names):
        print(f"  removed message: {name}")

    # --- shared-file walk ------------------------------------------------
    shared_files = sorted(old_names & new_names)
    top_level_diffs = 0
    defs_walked_names, defs_walked_blocks = set(), 0
    # aggregation key -> list of files
    findings = defaultdict(list)

    for fname in shared_files:
        old = load(old_dir / fname)
        new = load(new_dir / fname)

        # top level
        if as_set(old.get("required")) != as_set(new.get("required")):
            findings[("<root>", "top-level required changed",
                      f"{sorted(as_set(old.get('required')))} -> {sorted(as_set(new.get('required')))}")].append(fname)
            top_level_diffs += 1
        removed_props = set(old.get("properties", {})) - set(new.get("properties", {}))
        if removed_props:
            findings[("<root>", "top-level property removed", str(sorted(removed_props)))].append(fname)
            top_level_diffs += 1
        if old.get("type") != new.get("type"):
            findings[("<root>", "top-level type changed",
                      f"{old.get('type')} -> {new.get('type')}")].append(fname)
            top_level_diffs += 1

        old_defs = old.get("definitions", {})
        new_defs = new.get("definitions", {})
        defs_walked_names.update(old_defs)
        defs_walked_blocks += len(old_defs)

        for name in sorted(set(old_defs) - set(new_defs)):
            findings[(name, "definition removed", "")].append(fname)
        for name in sorted(set(new_defs) - set(old_defs)):
            findings[(name, "definition added", "")].append(fname)

        for name in sorted(set(old_defs) & set(new_defs)):
            d_old, d_new = old_defs[name], new_defs[name]
            req_old, req_new = as_set(d_old.get("required")), as_set(d_new.get("required"))
            for field in sorted(req_new - req_old):
                findings[(name, "field became required", field)].append(fname)
            for field in sorted(req_old - req_new):
                findings[(name, "field became optional", field)].append(fname)
            p_old = dict(walk_properties(d_old))
            p_new = dict(walk_properties(d_new))
            for prop in sorted(set(p_old) - set(p_new)):
                findings[(name, "property removed", prop)].append(fname)
            for prop in sorted(set(p_new) - set(p_old)):
                findings[(name, "property added (optional unless listed above)", prop)].append(fname)
            if d_old.get("type") != d_new.get("type"):
                findings[(name, "definition type changed",
                          f"{d_old.get('type')} -> {d_new.get('type')}")].append(fname)
            e_old, e_new = as_set(d_old.get("enum")), as_set(d_new.get("enum"))
            if e_old or e_new:
                added, removed = e_new - e_old, e_old - e_new
                if added:
                    findings[(name, f"enum values added (+{len(added)})",
                              str(sorted(added)))].append(fname)
                if removed:
                    findings[(name, f"enum values removed (-{len(removed)})",
                              str(sorted(removed)))].append(fname)
            for prop in set(p_old) & set(p_new):
                ml_old = p_old[prop].get("maxLength")
                ml_new = p_new[prop].get("maxLength")
                if ml_old is not None and ml_new is not None and ml_old != ml_new:
                    findings[(name, "maxLength changed", f"{prop}: {ml_old} -> {ml_new}")].append(fname)

    print(f"\ntop-level differences across {len(shared_files)} shared files: {top_level_diffs}")
    print(f"nested definitions walked (old side): {len(defs_walked_names)} distinct names, "
          f"{defs_walked_blocks} per-file inlined blocks")

    # --- findings, grouped ----------------------------------------------
    print(f"\nfindings across shared files ({len(findings)} distinct):")
    for (name, kind, detail), files in sorted(findings.items()):
        detail_part = f" [{detail}]" if detail else ""
        print(f"  {name}: {kind}{detail_part}")
        print(f"      in {len(files)} file(s): {', '.join(files)}")

    # --- digests ---------------------------------------------------------
    print("\ndirectory digests (sha256 over the sorted '<sha256>  <filename>' lines):")
    print(f"  old ({len(old_files)} files): {directory_digest(old_files)}")
    print(f"  new ({len(new_files)} files): {directory_digest(new_files)}")


if __name__ == "__main__":
    main()
