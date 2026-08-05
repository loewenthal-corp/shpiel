# hf-client: a real huggingface_hub / hf CLI environment used by e2e tests
# and the Tilt smoke job to prove unmodified HF tooling works against
# Shpiel. Never shipped.
FROM python:3.14-slim@sha256:a7fb1e634c4a578f9e0bd6327f11a3cde11b7a9395f48e24360c0988bcc5c2bc
RUN pip install --no-cache-dir "huggingface_hub[cli]>=0.26"
WORKDIR /work
ENTRYPOINT ["python"]
