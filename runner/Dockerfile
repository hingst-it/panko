FROM ghcr.io/opentofu/opentofu:1.11.7-minimal AS tofu
FROM python:3.12-alpine
COPY --from=tofu /usr/local/bin/tofu /usr/local/bin/tofu
WORKDIR /opt/module
COPY terraform/database/ ./
RUN tofu init -backend=false -input=false \
    && tofu providers mirror /opt/provider-mirror \
    && rm -rf .terraform
COPY runner/tofurc /etc/tofurc
COPY runner/entrypoint.py /opt/entrypoint.py
ENV TF_CLI_CONFIG_FILE=/etc/tofurc TF_IN_AUTOMATION=1 TF_INPUT=0 PYTHONUNBUFFERED=1
ENTRYPOINT ["python3", "/opt/entrypoint.py"]
