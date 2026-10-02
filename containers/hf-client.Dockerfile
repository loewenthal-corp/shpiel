# hf-client: a real huggingface_hub / hf CLI environment used by e2e tests
# and the Tilt smoke job to prove unmodified HF tooling works against
# Shpiel. Never shipped.
FROM python:3.14-slim@sha256:0741d101873c12ab927e6f8653feb8862b9bd58771177acb1b885b95141f91b4
RUN pip install --no-cache-dir "huggingface_hub[cli]>=0.26"
WORKDIR /work
ENTRYPOINT ["python"]
