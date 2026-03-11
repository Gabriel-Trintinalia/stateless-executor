SERVICE_NAME = "stateless-executor"
HTTP_PORT_ID = "http"
HTTP_PORT_NUM = 8080

DEFAULT_IMAGE = "ghcr.io/eth-proofs/stateless-executor:latest"
DEFAULT_GUEST_IMAGES = ["ghcr.io/eth-proofs/zevm-stateless:latest"]


def launch(
    plan,
    all_el_contexts,
    image=DEFAULT_IMAGE,
    guest_images=DEFAULT_GUEST_IMAGES,
    fork_name="",
    docker_host="",
):
    """Launch the stateless-executor service.

    The executor watches the EL pool for new block heads, fetches each block's
    RLP + execution witness, encodes them in the binary format expected by
    zevm-stateless guests, runs each guest image via Docker, and exposes:
      GET /metrics  — Prometheus exposition
      GET /results  — JSON array of the last 1000 verification results

    NOTE on Docker access: the executor calls `docker run` internally to
    execute guest images. When running inside Kurtosis the container cannot
    reach the host Docker socket by default. Two options:
      1. Pass docker_host="tcp://host.docker.internal:2375" to use a TCP
         Docker daemon reachable from within the enclave.
      2. For production deployments, mount /var/run/docker.sock via a
         docker-compose override or Kubernetes hostPath volume.

    Args:
        plan:             Kurtosis plan object
        all_el_contexts:  List of EL context objects (must have .rpc_http_url)
        image:            Executor docker image
        guest_images:     List of guest images to verify against each block
        fork_name:        Optional fork override passed to guests (e.g. "cancun")
        docker_host:      Optional DOCKER_HOST env var (e.g. "tcp://host.docker.internal:2375")

    Returns:
        Struct with service_name, ip_address, http_url, metrics_url, results_url
    """
    if len(guest_images) == 0:
        fail("stateless-executor: guest_images must not be empty")

    el_rpc_urls = ",".join([ctx.rpc_http_url for ctx in all_el_contexts])
    guest_images_str = ",".join(guest_images)

    env_vars = {
        "EL_RPC_URLS": el_rpc_urls,
        "GUEST_IMAGES": guest_images_str,
    }
    if fork_name != "":
        env_vars["FORK_NAME"] = fork_name
    if docker_host != "":
        env_vars["DOCKER_HOST"] = docker_host

    config = ServiceConfig(
        image=image,
        ports={
            HTTP_PORT_ID: PortSpec(
                number=HTTP_PORT_NUM,
                transport_protocol="TCP",
                application_protocol="http",
            ),
        },
        env_vars=env_vars,
    )

    service = plan.add_service(SERVICE_NAME, config)

    http_url = "http://{0}:{1}".format(service.ip_address, HTTP_PORT_NUM)
    plan.print("stateless-executor: {0}".format(http_url))

    return struct(
        service_name=SERVICE_NAME,
        ip_address=service.ip_address,
        http_url=http_url,
        metrics_url="{0}/metrics".format(http_url),
        results_url="{0}/results".format(http_url),
    )
