import ipaddress
import socket
from urllib.parse import urlparse


DENY_NETWORKS = [
    ipaddress.ip_network("127.0.0.0/8"),
    ipaddress.ip_network("10.0.0.0/8"),
    ipaddress.ip_network("172.16.0.0/12"),
    ipaddress.ip_network("192.168.0.0/16"),
    ipaddress.ip_network("169.254.0.0/16"),
    ipaddress.ip_network("::1/128"),
    ipaddress.ip_network("fc00::/7"),
    ipaddress.ip_network("fe80::/10"),
]


def _is_blocked_ip(ip: str) -> bool:
    addr = ipaddress.ip_address(ip)
    return any(addr in block for block in DENY_NETWORKS)


def validate_outbound_url(raw_url: str, allowed_domains: list[str]) -> None:
    parsed = urlparse(raw_url)
    if parsed.scheme != "https" or not parsed.hostname:
        raise ValueError("invalid_url")

    hostname = parsed.hostname.lower().strip()
    allowed = False
    for domain in allowed_domains:
        d = domain.lower().strip()
        if d and (hostname == d or hostname.endswith("." + d)):
            allowed = True
            break
    if not allowed:
        raise ValueError("domain_not_allowed")

    port = parsed.port or 443
    resolved = socket.getaddrinfo(hostname, port, proto=socket.IPPROTO_TCP)
    for info in resolved:
        ip = info[4][0]
        if _is_blocked_ip(ip):
            raise ValueError("blocked_destination")

