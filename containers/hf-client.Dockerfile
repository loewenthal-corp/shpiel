# hf-client: a real huggingface_hub / hf CLI environment used by e2e tests
# and the Tilt smoke job to prove unmodified HF tooling works against
# Shpiel. Never shipped.
FROM python:3.14-slim@sha256:b877e50bd90de10af8d82c57a022fc2e0dc731c5320d762a27986facfc3355c1
RUN pip install --no-cache-dir "huggingface_hub[cli]>=0.26"
WORKDIR /work
ENTRYPOINT ["python"]
