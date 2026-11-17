# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

"""Companion script for encryption_interop_pyarrow_test.go.

Usage:
  python3 encryption_interop_pyarrow.py probe
  python3 encryption_interop_pyarrow.py gen <outdir> <footer-key>
  python3 encryption_interop_pyarrow.py gen-kms <outdir>
  python3 encryption_interop_pyarrow.py read <file> <footer-key>
"""
import sys


def build_table():
    import pyarrow as pa

    return pa.table({
        "a": pa.array([1.0, 2.0, 3.0], pa.float64()),
        "b": pa.array([4.0, 5.0, 6.0], pa.float32()),
    })


def gen(outdir, footer_key):
    import os

    import pyarrow.parquet as pq
    from pyarrow.parquet.encryption import create_encryption_properties

    table = build_table()

    def write(name, fep):
        path = os.path.join(outdir, name)
        pq.write_table(table, path, encryption_properties=fep)
        print(name)

    write("uniform.parquet", create_encryption_properties(footer_key))
    write("ctr.parquet",
          create_encryption_properties(footer_key, encryption_algorithm="AES_GCM_CTR_V1"))
    write("aadprefix.parquet",
          create_encryption_properties(footer_key, aad_prefix=b"myspecialfile"))
    write("plainfooter_direct.parquet",
          create_encryption_properties(footer_key, plaintext_footer=True))


def gen_kms(outdir):
    """Write PKMT1 key material files with a CryptoFactory and a test-only
    in-memory KMS client whose wrapping is plain base64 (no cryptography
    dependency). The Go side mirrors the client to unwrap the keys."""
    import base64 as b64
    import os

    import pyarrow.parquet as pq
    import pyarrow.parquet.encryption as pe

    class Base64KmsClient(pe.KmsClient):
        def __init__(self, config):
            super().__init__()

        def wrap_key(self, key_bytes, master_key_identifier):
            return b64.b64encode(key_bytes).decode("ascii")

        def unwrap_key(self, wrapped_key, master_key_identifier):
            return b64.b64decode(wrapped_key)

    config = pe.KmsConnectionConfig(
        kms_instance_id="in-memory", kms_instance_url="local",
        key_access_token="token")

    def client_factory(kms_connection_config):
        return Base64KmsClient(kms_connection_config)

    factory = pe.CryptoFactory(client_factory)

    table = build_table()
    for name, double_wrapping in (("kms_double", True), ("kms_single", False)):
        encryption = pe.EncryptionConfiguration(
            footer_key="footer_master", column_keys={"column_master": ["a", "b"]},
            double_wrapping=double_wrapping)
        properties = factory.file_encryption_properties(config, encryption)
        path = os.path.join(outdir, name + ".parquet")
        pq.write_table(table, path, encryption_properties=properties)
        print(name)


def read(path, footer_key):
    import pyarrow.parquet as pq
    from pyarrow.parquet.encryption import create_decryption_properties

    table = pq.read_table(path, decryption_properties=create_decryption_properties(footer_key))
    for name, col in zip(table.column_names, table.columns):
        values = []
        for v in col.to_pylist():
            if isinstance(v, bool):
                values.append("1" if v else "0")
            else:
                values.append(str(v))
        print(name + "\t" + " ".join(values))


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else ""
    if mode == "probe":
        import pyarrow as pa
        from pyarrow.parquet.encryption import create_encryption_properties  # noqa: F401

        print(pa.__version__)
        return 0
    if mode == "gen":
        gen(sys.argv[2], sys.argv[3].encode())
        return 0
    if mode == "gen-kms":
        gen_kms(sys.argv[2])
        return 0
    if mode == "read":
        read(sys.argv[2], sys.argv[3].encode())
        return 0
    print("unknown mode: %r" % mode, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
