from contextlib import asynccontextmanager

from fastapi import FastAPI

from controller import SwitchboardController

controller = SwitchboardController()


@asynccontextmanager
async def lifespan(app: FastAPI):
    controller.start()
    yield
    controller.stop()


app = FastAPI(title="Switchboard Control API", lifespan=lifespan)


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}


@app.get("/status")
def status() -> dict:
    return controller.get_status()
