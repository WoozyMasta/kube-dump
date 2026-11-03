"""Convert GitHub-style alert blocks before MkDocs renders a page."""

import re

from mkdocs.plugins import event_priority


ALERT_RE = re.compile(
    r"^>\s*\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*$",
    re.IGNORECASE,
)
QUOTE_RE = re.compile(r"^>\s?(.*)$")
FENCE_RE = re.compile(r"^\s{0,3}(`{3,}|~{3,})")

MATERIAL_TYPES = {
    "note": "note",
    "tip": "tip",
    "important": "example",
    "warning": "warning",
    "caution": "danger",
}


@event_priority(0)
def on_files(files, config):
    """Hide the GitHub-only root README from the i18n file reconfiguration."""
    readme = files.get_file_from_path("README.md")
    if readme is not None and readme.inclusion.is_excluded():
        files.remove(readme)
    return files


@event_priority(60)
def on_page_markdown(markdown, page, config, files):
    """Convert standard GFM markers to localized Material admonitions."""
    i18n = config.plugins.get("i18n")
    translations = {}
    if i18n is not None:
        translations = i18n.current_language_config.admonition_translations or {}
        translations = {key.lower(): value for key, value in translations.items()}

    output = []
    fenced = None
    lines = markdown.splitlines()
    index = 0
    while index < len(lines):
        line = lines[index]
        match = ALERT_RE.match(line) if fenced is None else None
        if match:
            body = []
            next_index = index + 1
            while next_index < len(lines):
                body_match = QUOTE_RE.match(lines[next_index])
                if body_match is None:
                    break
                body.append(body_match.group(1))
                next_index += 1

            if body:
                alert_type = match.group(1).lower()
                title = translations.get(alert_type, "")
                heading = f"!!! {MATERIAL_TYPES[alert_type]}"
                output.append(f'{heading} "{title}"' if title else heading)
                output.extend(f"    {item}" if item else "    " for item in body)
                index = next_index
                continue

        output.append(line)
        fenced = _update_fence(fenced, line)
        index += 1

    return "\n".join(output)


def _update_fence(fenced, line):
    """Track fenced code blocks so alert-looking code remains unchanged."""
    match = FENCE_RE.match(line)
    if match is None:
        return fenced

    marker = match.group(1)
    if fenced is None:
        return marker[0], len(marker)
    if marker[0] == fenced[0] and len(marker) >= fenced[1]:
        return None
    return fenced
