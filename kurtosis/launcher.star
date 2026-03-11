SERVICE_NAME = "stateless-executor"
HTTP_PORT_ID = "http"
HTTP_PORT_NUM = 8080

DEFAULT_IMAGE = "ghcr.io/eth-proofs/stateless-executor:latest"

# Default guest: zevm-stateless binary inside the zevm-stateless image.
DEFAULT_GUESTS = [
    struct(
        image="ghcr.io/eth-proofs/zevm-stateless:latest",
        binary="/usr/local/bin/zevm-stateless",
    ),
]


def launch(
    plan,
    all_el_contexts,
    image=DEFAULT_IMAGE,
    guests=DEFAULT_GUESTS,
    fork_name="",
):
    """Launch the stateless-executor service.

    For each guest, the launcher pulls the guest image via plan.run_sh,
    extracts the binary into a Kurtosis artifact, and mounts it into the
    executor container. The executor runs the binary directly — no Docker
    daemon is needed inside the container.

    Args:
        plan:             Kurtosis plan object
        all_el_contexts:  List of EL context objects (must have .rpc_http_url)
        image:            Executor docker image
        guests:           List of structs with fields:
                            image  (str) — Docker image containing the guest binary
                            binary (str) — absolute path to the binary inside the image
        fork_name:        Optional fork override passed to guests (e.g. "cancun")

    Returns:
        Struct with service_name, ip_address, http_url, metrics_url, results_url
    """
    if len(guests) == 0:
        fail("stateless-executor: guests must not be empty")

    el_rpc_urls = ",".join([ctx.rpc_http_url for ctx in all_el_contexts])

    # Extract each guest binary from its image and collect mount info.
    files = {}
    guest_specs = []  # "name:/mount/path/binary"

    for i, guest in enumerate(guests):
        name = _short_name(guest.image)
        bin_name = guest.binary.split("/")[-1]
        mount_dir = "/guests/{0}".format(i)
        mount_path = "{0}/{1}".format(mount_dir, bin_name)

        artifact = plan.run_sh(
            name="extract-guest-{0}".format(name),
            description="Extracting {0} binary from {1}".format(bin_name, guest.image),
            image=guest.image,
            run="cp {0} /tmp/{1} && chmod +x /tmp/{1}".format(guest.binary, bin_name),
            store=["/tmp/{0}".format(bin_name)],
        )

        files[mount_dir] = artifact.files_artifacts[0]
        guest_specs.append("{0}:{1}".format(name, mount_path))

    env_vars = {
        "EL_RPC_URLS": el_rpc_urls,
        "GUEST_BINARIES": ",".join(guest_specs),
    }
    if fork_name != "":
        env_vars["FORK_NAME"] = fork_name

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
        files=files,
    )

    service = plan.add_service(SERVICE_NAME, config)

    # service.ports[HTTP_PORT_ID].url is the host-mapped URL shown in
    # `kurtosis enclave inspect` (e.g. http://127.0.0.1:56497).
    public_url = service.ports[HTTP_PORT_ID].url
    plan.print("stateless-executor")
    plan.print("  results: {0}/results".format(public_url))
    plan.print("  metrics: {0}/metrics".format(public_url))

    return struct(
        service_name=SERVICE_NAME,
        ip_address=service.ip_address,
        http_url=public_url,
        metrics_url="{0}/metrics".format(public_url),
        results_url="{0}/results".format(public_url),
    )


def _short_name(image):
    """ghcr.io/eth-proofs/zevm-stateless:latest -> zevm-stateless"""
    name = image
    if "/" in name:
        name = name.split("/")[-1]
    if ":" in name:
        name = name.split(":")[0]
    return name
