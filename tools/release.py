"""Release version and artifact checks, shared by the Release workflow and `make lint-chart`.

  release.py version TAG            print the artifact version of a tag (v0.0.2-rc.1 -> 0.0.2-rc.1)
  release.py stamp TAG              set the chart's version and appVersion to that version
  release.py images VERSION         print every image the release publishes
  release.py check VERSION MANIFEST check a rendering of the chart against the published names

The committed chart carries the release's core version (0.0.2); a release candidate tag stamps its
prerelease suffix into the chart before the CLI embeds it, so archives, chart and images all carry
the same version. Image and chart tags have no v prefix; git tags and binaries keep it.
"""
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parent.parent
CHART = ROOT / 'charts/panko/Chart.yaml'
VALUES = ROOT / 'charts/panko/values.yaml'
AGENT = ROOT / 'Dockerfile.agent'
REGISTRY = 'ghcr.io/hingst-it'
# Images built from this repository; the chart references the first two, agents use the third.
IMAGES = ['panko-server', 'panko-module-mirror', 'panko-agent']
CHART_REPOSITORY = f'oci://{REGISTRY}/charts'
SEMVER = r'(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?'


def version(tag):
    match = re.fullmatch('v' + SEMVER, tag)
    if not match:
        raise SystemExit(f'{tag!r} is not a release tag like v0.0.2 or v0.0.2-rc.1')
    return tag[1:]


def chart_field(text, field):
    match = re.search(rf'^{field}: *"?([^"\s]+)"?\s*$', text, re.M)
    if not match:
        raise SystemExit(f'{CHART} has no {field}')
    return match.group(1)


def stamp(tag):
    target = version(tag)
    core = target.split('-')[0]
    text = CHART.read_text()
    committed, app = chart_field(text, 'version'), chart_field(text, 'appVersion')
    if committed == app == target:
        print(target)
        return
    if committed != core or app != core:
        raise SystemExit(f'{CHART} has version {committed} and appVersion {app}; tag {tag} needs both at {core}')
    text = re.sub(r'^version: .*$', f'version: {target}', text, count=1, flags=re.M)
    text = re.sub(r'^appVersion: .*$', f'appVersion: "{target}"', text, count=1, flags=re.M)
    CHART.write_text(text)
    print(target)


def tofu(reference):
    match = re.fullmatch(r'ghcr\.io/opentofu/opentofu:(\d+\.\d+\.\d+)@(sha256:[0-9a-f]{64})', reference)
    if not match:
        raise SystemExit(f'OpenTofu image {reference!r} is not pinned to a version and digest')
    return match.groups()


def check(target, manifest):
    rendered = Path(manifest).read_text()
    errors = []
    found = set(re.findall(r'^\s*(?:-\s*)?image:\s*"?([^"\s]+)"?\s*$', rendered, re.M))
    found |= set(re.findall(r'"?(?:RUNNER_IMAGE|ENGINE_IMAGE|GIT_SYNC_IMAGE)"?,\s*value:\s*"?([^"\s},]+)', rendered))
    expected = {f'{REGISTRY}/panko-server:{target}', f'{REGISTRY}/panko-module-mirror:{target}'}
    for missing in sorted(expected - found):
        errors.append(f'the chart does not reference {missing}')
    for image in sorted(found):
        if image.startswith(REGISTRY + '/') and image not in expected:
            errors.append(f'the chart references {image}, which this release does not publish')
        name = image.split('@')[0]
        if ':' not in name.rsplit('/', 1)[-1] or name.endswith(':latest'):
            errors.append(f'{image} is not pinned to a version')
    # In-cluster Jobs and remote agents run the same OpenTofu build.
    engine = re.search(r'^  engine: *(\S+)', VALUES.read_text(), re.M).group(1)
    agent = re.search(r'^COPY --from=(ghcr\.io/opentofu/opentofu:\S+) ', AGENT.read_text(), re.M).group(1)
    if tofu(engine) != tofu(agent):
        errors.append(f'OpenTofu differs: chart engine {engine}, agent image {agent}')
    if engine not in found:
        errors.append(f'the rendered chart does not use the engine image {engine}')
    if errors:
        raise SystemExit('\n'.join(errors))
    print(f'ok chart {target}: ' + ', '.join(sorted(found)))


if __name__ == '__main__':
    command, args = (sys.argv[1], sys.argv[2:]) if len(sys.argv) > 1 else ('', [])
    if command == 'version' and len(args) == 1:
        print(version(args[0]))
    elif command == 'stamp' and len(args) == 1:
        stamp(args[0])
    elif command == 'images' and len(args) == 1:
        for image in IMAGES:
            print(f'{REGISTRY}/{image}:{args[0]}')
    elif command == 'check' and len(args) == 2:
        check(*args)
    else:
        raise SystemExit(__doc__)
