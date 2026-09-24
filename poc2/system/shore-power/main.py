from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException, Response
from pydantic import BaseModel, Field

from controller import ShorePowerController

controller = ShorePowerController()


@asynccontextmanager
async def lifespan(app: FastAPI):
    controller.start()
    yield
    controller.stop()


app = FastAPI(title="Shore Power Control API", lifespan=lifespan)


class ConnectRequest(BaseModel):
    connected: bool = Field(..., description="Whether shore power is plugged in and energized")


class PowerRequest(BaseModel):
    power_ratio: float = Field(
        ..., ge=0.0, le=1.0, description="Target power ratio of rated shore power, 0.0-1.0"
    )


@app.get("/health")
def health(response: Response) -> dict:
    health_data = controller.get_health()
    if health_data["status"] != "ok":
        response.status_code = 503
    return health_data


@app.get("/status")
def status() -> dict:
    return controller.get_status()


@app.post("/connect")
def set_connected(request: ConnectRequest) -> dict:
    controller.set_connected(request.connected)
    return {"connected": request.connected}


@app.get("/power")
def get_power() -> dict:
    status_data = controller.get_status()
    return {
        "target_power_ratio": status_data["target_power_ratio"],
        "current_power_ratio": status_data["current_power_ratio"],
    }


@app.post("/power")
def set_power(request: PowerRequest) -> dict:
    try:
        controller.set_target_power_ratio(request.power_ratio)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {"target_power_ratio": request.power_ratio}
