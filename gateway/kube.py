import json
from pathlib import Path
import ssl
import urllib.error
import urllib.request


class APIError(Exception):
    def __init__(self, code, body):
        self.code, self.body = code, body


class Kubernetes:
    def __init__(self):
        self.sa = Path('/var/run/secrets/kubernetes.io/serviceaccount')
        self.context = ssl.create_default_context(cafile=str(self.sa / 'ca.crt'))

    def request(self, method, path, body=None, content_type='application/json'):
        request = urllib.request.Request('https://kubernetes.default.svc' + path, method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={'Authorization': 'Bearer ' + (self.sa / 'token').read_text().strip(),
                     'Content-Type': content_type})
        try:
            with urllib.request.urlopen(request, context=self.context, timeout=15) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise APIError(error.code, json.load(error)) from error
        except (OSError, urllib.error.URLError) as error:
            raise APIError(503, {'message': 'Kubernetes API unavailable'}) from error
