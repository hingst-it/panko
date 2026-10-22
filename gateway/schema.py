"""CRD schemas are the only source of resource fields and documentation."""
from copy import deepcopy
import re


def json_schema(source):
    if isinstance(source, list):
        return [json_schema(item) for item in source]
    if not isinstance(source, dict):
        return source
    result = {key: json_schema(value) for key, value in source.items() if key != 'nullable'}
    if source.get('nullable'):
        return {'anyOf': [result, {'type': 'null'}]}
    return result


def patch_schema(source):
    result = deepcopy(source)
    if result.get('type') == 'object':
        required = result.pop('required', [])
        for key, value in result.get('properties', {}).items():
            partial = patch_schema(value)
            # A null merge-patch value removes an optional field (and resets its default).
            result['properties'][key] = partial if key in required else {'anyOf': [partial, {'type': 'null'}]}
    result.pop('default', None)
    return result


def resources(crds):
    result = {}
    for crd in crds:
        if crd['metadata'].get('labels', {}).get('platform.example.com/expose') != 'true':
            continue
        spec = crd['spec']
        if spec['scope'] != 'Namespaced':
            continue
        for version in spec['versions']:
            if not version.get('served'):
                continue
            root = version['schema']['openAPIV3Schema']
            if 'spec' not in root.get('properties', {}):
                continue
            resource = {'group': spec['group'], 'version': version['name'],
                        'kind': spec['names']['kind'], 'plural': spec['names']['plural'],
                        'schema': root}
            key = (resource['version'], resource['plural'])
            if key in result:
                raise ValueError(f'Ambiguous exposed API route: {key}')
            result[key] = resource
    return result


def openapi(discovered):
    schemas, paths = {}, {}
    schemas['Error'] = {'type': 'object', 'required': ['message', 'errors'], 'properties': {
        'message': {'type': 'string'}, 'errors': {'type': 'array', 'items': {'type': 'object',
            'required': ['field', 'message'], 'properties': {'field': {'type': 'string'}, 'message': {'type': 'string'}}}}}}
    for resource in discovered.values():
        root = json_schema(resource['schema'])
        properties = root['properties']
        identifier = re.sub(r'[^a-zA-Z0-9_]', '_', f"{resource['group']}_{resource['version']}_{resource['kind']}")
        name = {'type': 'string', 'description': 'Resource name, unique within this team.'}
        request = {'type': 'object', 'description': root.get('description', resource['kind']),
                   'required': ['name', 'spec'], 'additionalProperties': False,
                   'properties': {'name': name, 'spec': properties['spec']}}
        response = deepcopy(request)
        if 'status' in properties:
            response['properties']['status'] = properties['status']
        schemas[identifier + 'Create'] = request
        schemas[identifier + 'Patch'] = {'type': 'object', 'additionalProperties': False,
                                        'properties': {'spec': patch_schema(properties['spec'])}}
        schemas[identifier] = response
        schemas[identifier + 'List'] = {'type': 'array', 'items': {'$ref': f'#/components/schemas/{identifier}'}}
        collection = f"/{resource['version']}/teams/{{team}}/{resource['plural']}"
        for item in (False, True):
            path = collection + ('/{name}' if item else '')
            parameters = [{'name': 'team', 'in': 'path', 'required': True,
                           'schema': {'type': 'string'}, 'description': 'Team namespace authorized by the bearer token.'}]
            if item:
                parameters.append({'name': 'name', 'in': 'path', 'required': True, 'schema': name})
            methods = ['get', 'patch', 'delete'] if item else ['get', 'post']
            paths[path] = {}
            for method in methods:
                code = '201' if method == 'post' else '202' if method == 'delete' else '200'
                operation = {'operationId': f'{method}_{identifier}' + ('_item' if item else '_collection'),
                             'tags': [resource['kind']], 'summary': f'{method.upper()} {resource["kind"]}',
                             'parameters': parameters, 'security': [{'TeamToken': []}],
                             'responses': {code: {'description': 'Accepted' if method == 'delete' else 'Success'},
                                           **{str(c): {'description': label, 'content': {'application/json': {
                                               'schema': {'$ref': '#/components/schemas/Error'}}}}
                                              for c, label in [(401, 'Token required'), (403, 'Forbidden'),
                                                               (404, 'Not found'), (409, 'Conflict'),
                                                               (422, 'API validation failed'), (503, 'Kubernetes unavailable')]}}}
                if method != 'delete':
                    model = identifier + ('List' if method == 'get' and not item else '')
                    operation['responses'][code]['content'] = {'application/json': {'schema': {'$ref': '#/components/schemas/' + model}}}
                if method in ('post', 'patch'):
                    model = identifier + ('Create' if method == 'post' else 'Patch')
                    media = 'application/merge-patch+json' if method == 'patch' else 'application/json'
                    operation['requestBody'] = {'required': True, 'content': {
                        media: {'schema': {'$ref': '#/components/schemas/' + model}}}}
                paths[path][method] = operation
    return {'openapi': '3.1.0', 'info': {'title': 'Panko infrastructure API', 'version': '1.0.0',
            'description': 'Generated from exposed Kubernetes CRDs. The API server evaluates schema and CEL rules.'},
            'paths': paths, 'components': {'schemas': schemas, 'securitySchemes': {
                'TeamToken': {'type': 'http', 'scheme': 'bearer'}}}}
