#!/usr/bin/env python3
"""Add (or remove) the cpa-cursor block in CPA's live config.yaml.

The file is rewritten in place rather than replaced: CPA's watcher holds an
inotify watch on the inode, so a rename swap silently stops every later config
change from hot-reloading.

Usage: sudo ops/merge-config.py [--remove] [path]
"""

import sys

PLUGIN_ID = "cpa-cursor"
DEFAULT_PATH = "/var/lib/cli-proxy-api/config.yaml"

BLOCK = f"""    {PLUGIN_ID}:
      enabled: true
"""


def find_configs_section(lines: list[str]) -> int:
    """Return the index of the line after `plugins:` -> `configs:`."""
    in_plugins = False
    for index, line in enumerate(lines):
        if line.startswith("plugins:"):
            in_plugins = True
            continue
        if in_plugins:
            if line.strip() == "configs:":
                return index + 1
            if line and not line[0].isspace():
                break
    raise SystemExit("could not find plugins.configs in the config file")


def block_bounds(lines: list[str]) -> tuple[int, int] | None:
    marker = f"    {PLUGIN_ID}:"
    for index, line in enumerate(lines):
        if line.rstrip() == marker:
            end = index + 1
            while end < len(lines) and (not lines[end].strip() or lines[end].startswith("      ")):
                end += 1
            return index, end
    return None


def main() -> None:
    args = [arg for arg in sys.argv[1:] if arg != "--remove"]
    remove = "--remove" in sys.argv[1:]
    path = args[0] if args else DEFAULT_PATH

    with open(path, "r+", encoding="utf-8") as handle:
        lines = handle.read().splitlines(keepends=True)
        bounds = block_bounds(lines)

        if remove:
            if bounds is None:
                print(f"{PLUGIN_ID}: nothing to remove")
                return
            start, end = bounds
            lines[start:end] = []
        elif bounds is not None:
            print(f"{PLUGIN_ID}: already configured")
            return
        else:
            lines.insert(find_configs_section(lines), BLOCK)

        handle.seek(0)
        handle.write("".join(lines))
        handle.truncate()
    print(f"{PLUGIN_ID}: {'removed from' if remove else 'added to'} {path}")


if __name__ == "__main__":
    main()
