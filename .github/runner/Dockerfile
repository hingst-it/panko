FROM ghcr.io/actions/actions-runner:2.338.2

USER root
ARG GH_VERSION=2.103.2
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      build-essential ca-certificates curl git jq make nodejs \
      python3 python3-pip python3-venv unzip xz-utils zip \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /tmp/gh.tar.gz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" \
    && curl -fsSL -o /tmp/gh-checksums.txt "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_checksums.txt" \
    && grep " gh_${GH_VERSION}_linux_amd64.tar.gz$" /tmp/gh-checksums.txt | sed 's|gh_.*_linux_amd64.tar.gz|/tmp/gh.tar.gz|' | sha256sum -c - \
    && tar -xzf /tmp/gh.tar.gz -C /tmp \
    && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_amd64/bin/gh" /usr/local/bin/gh \
    && rm -rf /tmp/gh.tar.gz /tmp/gh-checksums.txt "/tmp/gh_${GH_VERSION}_linux_amd64"
RUN mkdir -p /opt/hostedtoolcache \
    && chown runner:docker /opt/hostedtoolcache
USER runner
