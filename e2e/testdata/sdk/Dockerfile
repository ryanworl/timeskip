FROM python:3.12-alpine
RUN pip install --no-cache-dir antithesis==0.4.0
COPY e2e_coverage.py /usr/local/lib/python3.12/site-packages/
COPY e2e-cov.sym.tsv /symbols/
COPY --chmod=755 parallel_driver_draw.py /opt/antithesis/test/v1/t/
COPY --chmod=755 parallel_driver_branch.py /opt/antithesis/test/v1/explore/
COPY --chmod=755 parallel_driver_tick.py eventually_end.py /opt/antithesis/test/v1/replan/
