from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

from controller import AuxLoadController

controller = AuxLoadController()


@asynccontextmanager
async def lifespan(app: FastAPI):
    controller.start()
    yield
    controller.stop()


app = FastAPI(title="Auxiliary Load API", lifespan=lifespan)


class LoadRequest(BaseModel):
    load_ratio: float = Field(..., ge=0.0, le=1.0, description="Target load ratio, 0.0-1.0")


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}


@app.get("/status")
def status() -> dict:
    return controller.get_status()


@app.get("/load")
def get_load() -> dict:
    status_data = controller.get_status()
    return {
        "target_load_ratio": status_data["target_load_ratio"],
        "current_load_ratio": status_data["current_load_ratio"],
    }


@app.post("/load")
def set_load(request: LoadRequest) -> dict:
    try:
        controller.set_target_load_ratio(request.load_ratio)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"target_load_ratio": request.load_ratio}
