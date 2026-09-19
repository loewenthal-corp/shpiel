# hf-client: a real huggingface_hub / hf CLI environment used by e2e tests
# and the Tilt smoke job to prove unmodified HF tooling works against
# Shpiel. Never shipped.
FROM python:3.14-slim@sha256:caaf356f40667c496d405780745b9ac25771c189a51dfcc42430d531ea09f8a2
RUN pip install --no-cache-dir "huggingface_hub[cli]>=0.26"
WORKDIR /work
ENTRYPOINT ["python"]
