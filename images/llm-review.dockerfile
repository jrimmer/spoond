# llm-review — LLM code review jobs (capability: llm-review). Pure Python stdlib + git.
FROM python:3.12-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
RUN mkdir -p /etc/spoond/init.d
