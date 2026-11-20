"""Build-time download of the pinned model revisions (the only network use)."""

from huggingface_hub import snapshot_download

for repo, rev in [
    ("intfloat/e5-base-v2", "f52bf8ec8c7124536f0efb74aca902b2995e5bcd"),
    ("cross-encoder/nli-deberta-v3-base", "6c749ce3425cd33b46d187e45b92bbf96ee12ec7"),
]:
    # Skip the duplicate onnx/openvino/tf/pytorch weights; safetensors is enough.
    snapshot_download(
        repo,
        revision=rev,
        allow_patterns=["*.json", "*.txt", "*.model", "*.safetensors"],
        ignore_patterns=["onnx/*", "openvino/*"],
    )
