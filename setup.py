import os
import sys

def iter_protos(parent=None):
    for root, _, files in os.walk('proto'):
        if not files:
            continue
        dest = root if not parent else os.path.join(parent, root)
        yield dest, [os.path.join(root, f) for f in files]

from setuptools import setup, find_packages

pkg_name = 'entroq'

setup(name=pkg_name,
      package_dir={
          '': 'contrib/py',
      },
      version='0.1.1',
      description='EntroQ Python Client Library',
      author='Chris Monson',
      author_email='shiblon@gmail.com',
      url='https://github.com/shiblon/entroq',
      license='Apache License, Version 2.0',
      packages=find_packages(),
      install_requires=[
          'setuptools==39.0.1',
          'grpcio==1.25.0',
          'grpcio-status==1.25.0',
          'grpcio-tools==1.25.0',
          'grpcio_health_checking==1.25.0',
          'protobuf==3.10.0',
      ],
      data_files=list(iter_protos(pkg_name)),
      py_modules=[
          'entroq.entroq_pb2',
          'entroq.entroq_pb2_grpc',
          'entroq',
      ])
