# hf-client: a real huggingface_hub / hf CLI environment used by e2e tests
# and the Tilt smoke job to prove unmodified HF tooling works against
# Shpiel. Never shipped.
FROM python:3.15-slim@sha256:65a65fc7b30b06cb6df4930fd4650697cb23f4c2698ab81fc87e4b031bc36928
RUN pip install --no-cache-dir "huggingface_hub[cli]>=0.26"
WORKDIR /work
ENTRYPOINT ["python"]
