from contextlib import asynccontextmanager

from fastapi import FastAPI, Response
from prometheus_fastapi_instrumentator import Instrumentator

from controller import SwitchboardController

controller = SwitchboardController()


@asynccontextmanager
async def lifespan(app: FastAPI):
    controller.start()
    yield
    controller.stop()


app = FastAPI(title="Switchboard Control API", lifespan=lifespan)
Instrumentator().instrument(app).expose(app)


@app.get("/health")
def health(response: Response) -> dict:
    health_data = controller.get_health()
    if health_data["status"] != "ok":
        response.status_code = 503
    return health_data


@app.get("/status")
def status() -> dict:
    return controller.get_status()


@app.get("/modbus")
def modbus_status() -> dict:
    """View the Modbus supply inputs used by the switchboard's allocation."""
    return controller.get_modbus_status()
