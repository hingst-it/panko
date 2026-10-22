from functools import lru_cache
import hmac
import json
import os
from pathlib import Path
from typing import Any
from urllib.parse import quote

from fastapi import Body, FastAPI, Request
from fastapi.responses import JSONResponse, Response
from fastapi.exceptions import RequestValidationError

from kube import APIError, Kubernetes
from schema import openapi, resources

app = FastAPI(title='Panko infrastructure API', openapi_url=None, docs_url=None, redoc_url=None)


@lru_cache
def kubernetes():
    return Kubernetes()


def discover():
    result = kubernetes().request('GET', '/apis/apiextensions.k8s.io/v1/customresourcedefinitions?labelSelector=platform.example.com%2Fexpose%3Dtrue')
    return resources(result['items'])


def error(code, message, errors=None):
    return JSONResponse(status_code=code, content={'message': message, 'errors': errors or []},
                        headers={'WWW-Authenticate': 'Bearer'} if code == 401 else None)


@app.exception_handler(APIError)
def api_error(request, exception):
    code = 422 if exception.code in (400, 422) else exception.code
    causes = exception.body.get('details', {}).get('causes', [])
    fields = [{'field': c.get('field', '').replace('metadata.name', 'name'),
               'message': c.get('message', exception.body.get('message', 'Validation failed'))} for c in causes]
    return error(code, exception.body.get('message', 'Kubernetes request failed'), fields)


@app.exception_handler(RequestValidationError)
def request_error(request, exception):
    return error(422, 'Invalid request body', [{'field': '.'.join(map(str, e['loc'])), 'message': e['msg']}
                                            for e in exception.errors()])


@app.get('/healthz')
def health():
    return {'ok': True}


@app.get('/openapi.json')
def document():
    try:
        return openapi(discover())
    except ValueError as exception:
        return error(503, str(exception))


@app.get('/docs')
def documentation():
    from fastapi.openapi.docs import get_swagger_ui_html
    return get_swagger_ui_html(openapi_url='/openapi.json', title='Panko API')


def authorize(request, team):
    tokens = json.loads(Path(os.getenv('TEAM_TOKENS_FILE', '/etc/panko/tokens.json')).read_text())
    header = request.headers.get('Authorization', '')
    if not header.startswith('Bearer '):
        return error(401, 'A team bearer token is required')
    candidate = header[7:]
    authenticated = next((name for name, token in tokens.items() if hmac.compare_digest(candidate, token)), None)
    if authenticated is None:
        return error(401, 'Invalid bearer token')
    if authenticated != team:
        return error(403, 'This token does not authorize the requested team')
    return None


def view(resource):
    result = {'name': resource['metadata']['name'], 'spec': resource['spec']}
    if 'status' in resource:
        result['status'] = resource['status']
    return result


def handle(request, version, team, plural, name=None, payload=None):
    denied = authorize(request, team)
    if denied is not None:
        return denied
    try:
        resource = discover().get((version, plural))
    except ValueError as exception:
        return error(503, str(exception))
    if not resource:
        return error(404, 'No exposed resource at this route')
    collection = f"/apis/{quote(resource['group'], safe='')}/{quote(version, safe='')}/namespaces/{quote(team, safe='')}/{quote(plural, safe='')}"
    path = collection + ('/' + quote(name, safe='') if name else '')
    method = request.method
    if method in ('POST', 'PATCH'):
        if not isinstance(payload, dict):
            return error(422, 'Expected a JSON object', [{'field': 'body', 'message': 'Expected a JSON object'}])
        allowed = {'name', 'spec'} if method == 'POST' else {'spec'}
        unknown = payload.keys() - allowed
        if unknown:
            return error(422, 'Unexpected fields', [{'field': k, 'message': 'Field is not accepted'} for k in sorted(unknown)])
        if method == 'POST':
            if not isinstance(payload.get('name'), str) or not payload['name']:
                return error(422, 'name is required', [{'field': 'name', 'message': 'Expected a nonempty string'}])
            if 'spec' not in payload:
                return error(422, 'spec is required', [{'field': 'spec', 'message': 'Required field'}])
            body = {'apiVersion': f"{resource['group']}/{version}", 'kind': resource['kind'],
                    'metadata': {'name': payload['name'], 'namespace': team}, 'spec': payload['spec']}
        else:
            body = payload
        content_type = 'application/merge-patch+json' if method == 'PATCH' else 'application/json'
        result = kubernetes().request(method, path + '?fieldValidation=Strict', body, content_type)
        return JSONResponse(view(result), status_code=201 if method == 'POST' else 200)
    if method == 'DELETE':
        kubernetes().request('DELETE', path, {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'propagationPolicy': 'Background'})
        return Response(status_code=202)
    result = kubernetes().request('GET', path)
    return [view(item) for item in result['items']] if name is None else view(result)


@app.api_route('/{version}/teams/{team}/{plural}', methods=['GET', 'POST'])
def collection(request: Request, version: str, team: str, plural: str, payload: Any = Body(default=None)):
    return handle(request, version, team, plural, payload=payload)


@app.api_route('/{version}/teams/{team}/{plural}/{name}', methods=['GET', 'PATCH', 'DELETE'])
def item(request: Request, version: str, team: str, plural: str, name: str, payload: Any = Body(default=None)):
    return handle(request, version, team, plural, name, payload)
