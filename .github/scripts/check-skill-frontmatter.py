"""Fail when a SKILL.md has no YAML frontmatter, or it is not a mapping."""
import sys

import yaml

failed = False
for path in sys.argv[1:]:
    try:
        parts = open(path, encoding="utf-8").read().split("---\n", 2)
        if parts[0] != "" or len(parts) < 3:
            raise ValueError("no frontmatter")
        if not isinstance(yaml.safe_load(parts[1]), dict):
            raise ValueError("frontmatter is not a mapping")
    except (ValueError, yaml.YAMLError) as e:
        print(f"{path}: {e}", file=sys.stderr)
        failed = True
sys.exit(1 if failed else 0)
