"""Render the Homebrew formula from the checksums of an existing release."""
from pathlib import Path
import re
import sys


def formula(version, checksums):
    if not re.fullmatch(r'(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?', version):
        raise ValueError('expected a release version without the v prefix')
    hashes = {}
    for line in checksums.splitlines():
        digest, name = line.split()
        if not re.fullmatch(r'[0-9a-f]{64}', digest):
            raise ValueError('invalid SHA-256 digest')
        hashes[name] = digest
    lines = [
        'class Panko < Formula',
        '  desc "Self-service infrastructure API powered by OpenTofu and Kubernetes"',
        '  homepage "https://github.com/hingst-it/panko"',
        f'  version "{version}"',
        '  license "Apache-2.0"',
        '',
    ]
    for brew_os, target_os in [('macos', 'darwin'), ('linux', 'linux')]:
        lines.append(f'  on_{brew_os} do')
        for brew_arch, arch in [('arm', 'arm64'), ('intel', 'amd64')]:
            name = f'panko_{version}_{target_os}_{arch}.tar.gz'
            digest = hashes[name]
            lines += [
                f'    on_{brew_arch} do',
                f'      url "https://github.com/hingst-it/panko/releases/download/v{version}/{name}"',
                f'      sha256 "{digest}"',
                '    end',
            ]
        lines += ['  end', '']
    lines += [
        '  def install',
        '    bin.install "panko", "panko-admin"',
        '  end',
        '',
        '  test do',
        f'    assert_equal "panko v{version}", shell_output("#{{bin}}/panko --version").strip',
        f'    assert_equal "panko-admin v{version}", shell_output("#{{bin}}/panko-admin --version").strip',
        '  end',
        'end',
    ]
    return '\n'.join(lines) + '\n'


if __name__ == '__main__':
    if len(sys.argv) != 3:
        raise SystemExit('usage: release_homebrew.py VERSION SHA256SUMS')
    print(formula(sys.argv[1], Path(sys.argv[2]).read_text()), end='')
