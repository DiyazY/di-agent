from contextlib import asynccontextmanager

from fastapi import FastAPI, Response

from controller import SwitchboardController

controller = SwitchboardController()


@asynccontextmanager
async def lifespan(app: FastAPI):
    controller.start()
    yield
    controller.stop()


app = FastAPI(title="Switchboard Control API", lifespan=lifespan)


@app.get("/health")
def health(response: Response) -> dict:
    health_data = controller.get_health()
    if health_data["status"] != "ok":
        response.status_code = 503
    return health_data


@app.get("/status")
def status() -> dict:
    return controller.get_status()
